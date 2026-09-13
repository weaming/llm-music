package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// tap-player 消费 llm-server 的 /v1/tap 旁路，把 AI 字符流实时音乐化。
//
// 行为与 Python 版（tap-player/）等价：同一套常量、同一套合成数学、同一份 jsonl 格式。
// 区别只在实现语言——这个版本是常驻用的。

const (
	// 事件队列容量。满了丢最旧再塞新的，绝不阻塞网络读取。
	eventQueueCap = 256
	defaultRecord = "tmp/tap.jsonl"
	defaultWAV    = "tmp/tap.wav"
	// 重放时的事件推送节拍。
	//
	// 注意：macOS 的定时器/sleep 最小粒度约 1–2ms，所以 replay 的 -speed
	// 过大时会跟不上（实测 speed=2 能推完 96%，speed=6 只有 56%）。
	// 这只影响加速播放；play 是网络事件驱动、没有时间轴，不受影响。
	pumpTick = 2 * time.Millisecond
)

func main() {
	initTimezone()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "play":
		runPlay(os.Args[2:])
	case "replay":
		runReplay(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `tap-player — 消费 /v1/tap，把 AI 字符流实时音乐化

用法:
  tap-player play   [选项]              连 tap 实时播放
  tap-player replay [选项] <file.jsonl> 重放素材

play 选项:
  -tap-url URL    tap 端点（默认 `+DefaultTapURL+`）
  -bpm N          速度（默认 `+fmt.Sprint(DefaultBPM)+`）
  -root MIDI      主音 MIDI 音高（默认 `+fmt.Sprint(DefaultRootMIDI)+` = D4）
  -volume F       音量倍数（默认 1.0）
  -record PATH    同时录制到 jsonl（空则不录）
  -quiet          不打印每秒状态行

replay 选项:
  与 play 相同，另加:
  -speed F        倍速（默认 1.0）
  -max-gap F      多余空闲压缩到该秒数（默认 `+fmt.Sprint(DefaultMaxGapSec)+`）
  -no-audio       不播放，渲染成 WAV
  -out PATH       --no-audio 的输出路径（默认 `+defaultWAV+`）
`)
}

// commonFlags 是 play 与 replay 共用的参数。
type commonFlags struct {
	tapURL string
	bpm    int
	root   int
	volume float64
	quiet  bool
}

func registerCommon(fs *flag.FlagSet, settings Settings) *commonFlags {
	flags := &commonFlags{}
	fs.StringVar(&flags.tapURL, "tap-url", settings.TapURL, "tap 端点")
	fs.IntVar(&flags.bpm, "bpm", settings.BPM, "速度")
	fs.IntVar(&flags.root, "root", settings.RootMIDI, "主音 MIDI 音高")
	fs.Float64Var(&flags.volume, "volume", settings.Volume, "音量倍数")
	fs.BoolVar(&flags.quiet, "quiet", false, "不打印每秒状态行")
	return flags
}

// apply 把命令行参数覆盖到配置上。
func (f *commonFlags) apply(settings Settings) Settings {
	settings.TapURL = f.tapURL
	settings.BPM = f.bpm
	settings.RootMIDI = f.root
	settings.Volume = f.volume
	return settings
}

// runPlay 连 tap 实时播放，可选同时录制。
func runPlay(args []string) {
	fs := flag.NewFlagSet("play", flag.ExitOnError)
	settings := defaultSettings()
	flags := registerCommon(fs, settings)
	recordPath := fs.String("record", "", "同时录制到 jsonl（空则不录）")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	settings = flags.apply(settings)
	logQuiet = flags.quiet

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engine := newEngine(settings.SampleRate, settings.BPM, settings.RootMIDI, 0)
	events := make(chan TapEvent, eventQueueCap)

	var recorder *Recorder
	if *recordPath != "" {
		var err error
		recorder, err = newRecorder(*recordPath)
		if err != nil {
			logFatalf("record_open_failed", "%v", err)
		}
		defer recorder.Close()
		logEvent("recording", "path", *recordPath)
		if recorder.AppendedToExisting() {
			logEvent("recording_appending", "path", *recordPath, "note", "文件已有内容，本次录制会追加到其后")
		}
	}

	client := newTapClient(settings.TapURL)
	go func() {
		<-ctx.Done()
		client.Stop()
	}()
	go client.Run(ctx, func(event TapEvent) {
		if recorder != nil {
			if err := recorder.Write(event); err != nil {
				logEvent("record_write_failed", "error", err.Error())
			}
		}
		publishEvent(events, event)
	})

	if err := runRealtime(ctx, engine, events, settings); err != nil {
		logFatalf("audio_failed", "%v", err)
	}
	if recorder != nil {
		logEvent("record_finished", "path", *recordPath, "count", recorder.Count(), "gaps", client.Gaps())
	}
}

// runReplay 重放 jsonl。--no-audio 时离线渲染成 WAV。
func runReplay(args []string) {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	settings := defaultSettings()
	flags := registerCommon(fs, settings)
	speed := fs.Float64("speed", 1.0, "倍速")
	maxGap := fs.Float64("max-gap", DefaultMaxGapSec, "多余空闲压缩到该秒数")
	noAudio := fs.Bool("no-audio", false, "不播放，渲染成 WAV")
	out := fs.String("out", defaultWAV, "--no-audio 的输出路径")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "replay 需要一个 jsonl 文件参数")
		os.Exit(2)
	}
	settings = flags.apply(settings)
	logQuiet = flags.quiet

	path := fs.Arg(0)
	timed, err := timedEvents(path, *speed, *maxGap)
	if err != nil {
		logFatalf("replay_read_failed", "读取 %s 失败: %v", path, err)
	}
	duration := 0.0
	if len(timed) > 0 {
		duration = timed[len(timed)-1].offset
	}
	logEvent("replay_loaded", "path", path, "events", len(timed), "duration", round2(duration))

	engine := newEngine(settings.SampleRate, settings.BPM, settings.RootMIDI, 0)

	if *noAudio {
		if dir := filepath.Dir(*out); dir != "" {
			os.MkdirAll(dir, 0o755)
		}
		frames, err := renderToWAV(timed, engine, *out, offlineBlock)
		if err != nil {
			logFatalf("render_failed", "%v", err)
		}
		logEvent("rendered", "path", *out, "frames", frames,
			"seconds", round2(float64(frames)/float64(settings.SampleRate)))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	events := make(chan TapEvent, eventQueueCap)
	go func() {
		// 按绝对时间轴推进，且**批量推送**：逐事件 time.Sleep 在毫秒级间隔上
		// 有累积误差，加速播放时（间隔约 1ms）会明显跟不上。
		// 用 ticker 对齐时间，每次把已到期的事件一次推完。
		started := time.Now()
		ticker := time.NewTicker(pumpTick)
		defer ticker.Stop()

		index := 0
		for index < len(timed) {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			elapsed := time.Since(started).Seconds()
			for index < len(timed) && timed[index].offset <= elapsed {
				publishEvent(events, timed[index].event)
				index++
			}
		}

		// 放完不要立刻停：留出尾巴让终止式与混响自然收掉。
		// 直接取消会把最后一个音切掉（离线渲染有 tailSec 的尾巴，这里对齐）。
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(tailSec * float64(time.Second))):
		}
		stop()
	}()

	if err := runRealtime(ctx, engine, events, settings); err != nil {
		logFatalf("audio_failed", "%v", err)
	}
}

// publishEvent 把事件推进队列。队列满时丢最旧再塞新的——
// 实时场景要"此刻"，积压比丢失更糟；而且绝不阻塞网络读取。
func publishEvent(events chan TapEvent, event TapEvent) {
	select {
	case events <- event:
		return
	default:
	}
	select {
	case <-events:
	default:
	}
	select {
	case events <- event:
	default:
	}
}
