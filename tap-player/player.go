package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"time"

	"github.com/ebitengine/oto/v3"
)

// 音频输出与离线渲染。两者共用同一个 Engine，区别只在"谁来推进时钟"：
// 实时模式由声卡拉数据，离线模式由虚拟时间轴推进。

const (
	offlineBlock = 1024
	// 重放结束后多渲染这么久，让终止式与混响自然收掉。
	// 不留的话末尾那个 2.4 秒的终止式会被硬切，而导出的 WAV 却是完整的，
	// 同一份素材听起来会不一样。
	tailSec   = 2.5
	pcmMax    = 32767
	statusSec = 1.0
)

// timedEvent 是摊平到相对时间轴上的事件。
type timedEvent struct {
	event  TapEvent
	offset float64
}

// processEvent 把一个 tap 事件喂给状态跟踪器。
func processEvent(event TapEvent, tracker *StateTracker, now float64) {
	if event.Kind == kindError {
		// 错误有自己的动机，不复用终止式 —— 两者听感必须可分辨，
		// 否则"答完了"和"出错了"在音乐上是同一件事。
		logEvent("upstream_error", "status", event.Status, "detail", shortError(event.Raw, 200))
		tracker.FeedLifecycle(kindError, now)
		return
	}
	if event.Kind != kindFrame {
		tracker.FeedLifecycle(event.Kind, now)
		return
	}
	if delta, ok := parseDelta(event.Raw); ok {
		tracker.FeedDelta(delta, now)
	} else {
		tracker.Tick(now)
	}
}

// drainEvents 把队列里积压的事件全部吃掉。非阻塞，供音频回调使用。
func drainEvents(events <-chan TapEvent, tracker *StateTracker, now float64) {
	for {
		select {
		case event, open := <-events:
			if !open {
				return
			}
			processEvent(event, tracker, now)
		default:
			return
		}
	}
}

// timedEvents 把事件流摊平成（事件, 相对秒）序列。
//
// tap 的 ts 是墙钟时间，两次请求之间可能隔很久；maxGap 把这类空闲压缩掉，
// speed 再整体倍速，这样调音色时不必等真实间隔。
func timedEvents(path string, speed, maxGap float64) ([]timedEvent, error) {
	if speed <= 0 {
		speed = 0.01
	}

	var timed []timedEvent
	var previous time.Time
	hasPrevious := false
	offset := 0.0

	skipped, err := readEvents(path, func(event TapEvent) {
		if stamp, ok := parseTimestamp(event.Ts); ok {
			if hasPrevious {
				gap := stamp.Sub(previous).Seconds()
				if gap < 0 {
					gap = 0
				}
				offset += min(gap, maxGap) / speed
			}
			previous = stamp
			hasPrevious = true
		}
		timed = append(timed, timedEvent{event: event, offset: offset})
	})
	if err != nil {
		return nil, err
	}
	if skipped > 0 {
		logEvent("replay_skipped_lines", "path", path, "skipped", skipped)
	}
	return timed, nil
}

// parseTimestamp 解析 tap 的 RFC3339 时间戳（带时区偏移）。
func parseTimestamp(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	stamp, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false
	}
	return stamp, true
}

// synthReader 是 oto 的 PCM 供给源。Read 在 oto 的 goroutine 里被调用。
type synthReader struct {
	engine  *Engine
	tracker *StateTracker
	events  <-chan TapEvent
	volume  float32
	started time.Time
}

// Read 生成 PCM 字节。oto 用 pull 模型按自己的节奏要数据——
// 这正是"时钟独立于事件到达"的天然实现：token 的突发性进不了音频时序。
func (r *synthReader) Read(p []byte) (int, error) {
	// 立体声 float32：每帧 8 字节
	frames := len(p) / 8
	if frames == 0 {
		return 0, nil
	}

	now := time.Since(r.started).Seconds()
	drainEvents(r.events, r.tracker, now)
	r.engine.SetState(r.tracker.Tick(now))

	out := r.engine.Render(frames)
	for i, sample := range out {
		binary.LittleEndian.PutUint32(p[i*4:], math.Float32bits(sample*r.volume))
	}
	return frames * 8, nil
}

// startStatusReporter 每秒输出一行状态，供调音色与观察。
//
// 刻意用独立 goroutine + wall clock，而**不是**在 Read 里判断：
// oto 是 pull 模型且会提前缓冲，Read 的调用速率与音频时长完全不成比例，
// 在 Read 里做时间判断要么不触发、要么瞬间刷屏。
func startStatusReporter(ctx context.Context, engine *Engine, tracker *StateTracker) {
	if logQuiet {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Duration(statusSec * float64(time.Second)))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state := engine.State()
				// IdleSec 在"从未有内容"时是 +Inf，而 json.Marshal 拒绝 Inf，
				// 用 -1 表示这个状态
				idle := state.IdleSec
				if math.IsInf(idle, 1) {
					idle = -1
				}
				logEvent("state",
					"phase", state.Phase,
					"density", round2(state.Density),
					"energy", round2(state.Energy),
					"idle_sec", round2(idle),
					"tokens", tracker.Tokens(),
					"notes", engine.ActiveNotes(),
				)
			}
		}
	}()
}

// runRealtime 阻塞式实时播放，直到 ctx 取消。
func runRealtime(ctx context.Context, engine *Engine, events <-chan TapEvent, settings Settings) error {
	// 整个进程只能有一个 oto Context，所以这里创建后一直用到退出
	audioCtx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   settings.SampleRate,
		ChannelCount: 2,
		Format:       oto.FormatFloat32LE,
	})
	if err != nil {
		return fmt.Errorf("初始化音频失败: %w", err)
	}
	<-ready

	reader := &synthReader{
		engine:  engine,
		tracker: newStateTracker(),
		events:  events,
		volume:  float32(settings.Volume),
		started: time.Now(),
	}
	startStatusReporter(ctx, engine, reader.tracker)

	player := audioCtx.NewPlayer(reader)
	player.Play()
	logEvent("audio_started",
		"sample_rate", settings.SampleRate,
		"bpm", settings.BPM,
		"volume", settings.Volume,
	)

	<-ctx.Done()

	// oto 的 Player 在不可达时会被自动关闭（见其文档：
	// "A player must be kept reachable as long as it should keep playing"）。
	// 它是局部变量，GC 完全可以在上面那个阻塞期间判定它已死 → 播放中途静音
	// （实测表现为 Read 只被调用两次就不再被调用）。
	// KeepAlive 保证从 Play 到这里它一直是活的。
	runtime.KeepAlive(player)

	player.Close()
	logEvent("audio_stopped")
	return nil
}

// renderToWAV 离线渲染成 16-bit 立体声 WAV，返回总采样帧数。
// 用虚拟时钟推进，不做真实等待，所以比实时快得多。
func renderToWAV(timed []timedEvent, engine *Engine, path string, block int) (int, error) {
	file, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("创建 WAV 失败: %w", err)
	}
	defer file.Close()

	if err := writeWAVHeader(file, engine.sampleRate, 0); err != nil {
		return 0, err
	}

	tracker := newStateTracker()
	cursor := 0.0
	rate := float64(engine.sampleRate)
	totalFrames := 0
	pcm := make([]byte, 0, block*4)

	emit := func(frames int) error {
		out := engine.Render(frames)
		pcm = pcm[:0]
		for _, sample := range out {
			clamped := max(-1.0, min(1.0, float64(sample)))
			pcm = binary.LittleEndian.AppendUint16(pcm, uint16(int16(clamped*pcmMax)))
		}
		if _, err := file.Write(pcm); err != nil {
			return err
		}
		totalFrames += frames
		return nil
	}

	for _, item := range timed {
		// 事件之间的空档也要推进状态：静默判定依赖 idleSec 随时间增长，
		// 只渲染不 tick 的话状态会停在事件当时，离线渲染就不会安静下来。
		for cursor < item.offset {
			frames := min(block, max(1, int((item.offset-cursor)*rate)))
			engine.SetState(tracker.Tick(cursor))
			if err := emit(frames); err != nil {
				return totalFrames, err
			}
			cursor += float64(frames) / rate
		}
		processEvent(item.event, tracker, cursor)
		engine.SetState(tracker.Tick(cursor))
	}

	// 尾巴：让最后的状态与混响自然衰减
	remaining := int(tailSec * rate)
	for remaining > 0 {
		frames := min(block, remaining)
		engine.SetState(tracker.Tick(cursor))
		if err := emit(frames); err != nil {
			return totalFrames, err
		}
		cursor += float64(frames) / rate
		remaining -= frames
	}

	if err := writeWAVHeader(file, engine.sampleRate, totalFrames); err != nil {
		return totalFrames, err
	}
	return totalFrames, nil
}

// writeWAVHeader 写 44 字节的 WAV 头。
// 渲染完成后用同样的调用回填长度（dataFrames 传实际帧数）。
func writeWAVHeader(w io.WriteSeeker, sampleRate int, dataFrames int) error {
	const (
		channels      = 2
		bitsPerSample = 16
	)
	byteRate := sampleRate * channels * bitsPerSample / 8
	dataBytes := dataFrames * channels * bitsPerSample / 8
	header := make([]byte, 0, 44)

	header = append(header, "RIFF"...)
	header = binary.LittleEndian.AppendUint32(header, uint32(36+dataBytes))
	header = append(header, "WAVEfmt "...)
	header = binary.LittleEndian.AppendUint32(header, 16)                       // fmt chunk 大小
	header = binary.LittleEndian.AppendUint16(header, 1)                        // PCM
	header = binary.LittleEndian.AppendUint16(header, channels)                 //
	header = binary.LittleEndian.AppendUint32(header, uint32(sampleRate))       //
	header = binary.LittleEndian.AppendUint32(header, uint32(byteRate))         //
	header = binary.LittleEndian.AppendUint16(header, channels*bitsPerSample/8) // block align
	header = binary.LittleEndian.AppendUint16(header, bitsPerSample)            //
	header = append(header, "data"...)
	header = binary.LittleEndian.AppendUint32(header, uint32(dataBytes))

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := w.Write(header)
	return err
}

// round2 保留两位小数，只用于日志可读性。
func round2(value float64) float64 {
	return math.Round(value*100) / 100
}
