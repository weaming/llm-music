package main

import (
	"math"
	"math/rand/v2"
	"sync/atomic"
)

// 合成引擎：独立时钟驱动的实时音频渲染。
//
// 关键设计：**时钟与事件到达完全解耦**。引擎按固定的 16 分音符网格推进，
// 在每个网格点读一次当前状态决定是否发声；事件只在另一个线程里更新状态。
// LLM token 的突发性因此不进入音频时序——这是"数据控制密度、不控制位置"的落地。
//
// 状态用 atomic.Pointer 整体替换，回调只读一次引用，所以渲染路径不需要任何锁。

const (
	// 16 分音符网格：一小节 16 格
	gridPerBeat = 4
	beatsPerBar = 4
	gridPerBar  = gridPerBeat * beatsPerBar

	// 基础触发概率的下限（有输入但密度很低时的稀疏触发）与密度带来的增量。
	// 注意这不再是"静默时也有声"——完全无输入超过 silenceAfterBeats 就不发声了。
	baseTriggerChance  = 0.08
	densityTriggerGain = 0.72

	// 持续无输入多少拍之后彻底静默。按拍计（而非秒），这样换 BPM 也成立。
	// 8 拍 = 4/4 的两小节，够覆盖正常的 token 间隔，又不会空转太久。
	silenceAfterBeats = 8

	// 尾部淡出时长，避免音符在非零幅度上被截断产生爆音
	tailFadeSec = 0.012

	// 错误动机：音之间的间隔与单音时值
	errorNoteGapSec      = 0.28
	errorNoteDurationSec = 0.5

	// 混响湿声比例随能量变化
	reverbWetBase = 0.18
	reverbWetGain = 0.6

	// 输出限幅
	maxAmplitude = 0.85

	// 侧链压缩：鼓点出现时把旋律压低，给它让出空间。
	// 这是 EDM 混音的标志性手法——底鼓之所以"跳出来"，靠的不只是音量大，
	// 更是别的声部在那一刻退开了。没有这一层，鼓点只是"混进去"。
	duckDepth      = 0.35
	duckAttackSec  = 0.005
	duckRecoverSec = 0.09

	// 旋律总线增益。鼓点要"有气势"就得在混音里主导——但直接放大鼓会撞限幅，
	// 所以把旋律让下来，两者一起动作才既有对比度又不削波。
	melodyGain = 0.72
)

// phaseStride 是各相位每隔几个网格点才考虑发声——实现"同类靠节奏区分"。
var phaseStride = map[string]int{
	phaseIdle:      8,
	phaseReasoning: 4,
	phaseProse:     2,
	phaseCode:      1,
}

// activeNote 是一个正在发声的音符。时间是全局采样位置。
type activeNote struct {
	start        int
	duration     int
	freq         float64
	velocity     float64
	harmonics    []float64
	attack       int
	decay        int
	isPercussive bool
	// ducks 标记的音符走独立总线，并在出现时压低其他声部（侧链）
	ducks bool
	// 音高下滑：起音瞬间频率是 freq·(1+pitchDrop)，按 dropTau 指数滑回 freq
	pitchDrop float64
	dropTau   float64
}

// Engine 把 SynthState 渲染成 interleaved 立体声波形。
type Engine struct {
	sampleRate   int
	rootMIDI     int
	rng          *rand.Rand
	silenceAfter float64

	// state 由事件线程整体替换、渲染路径读一次引用
	state atomic.Pointer[SynthState]

	sampleClock int64
	gridSamples int64
	nextGrid    int64
	gridIndex   int64

	notes  []activeNote
	reverb *Reverb
	// 鼓专用的短反射。与旋律的长混响分开——共用长混响会把鼓的瞬态抹开，
	// 听感变成"远处放烟花"；完全干声又"死"。
	drumReverb *Reverb

	// 供状态行跨线程读取，避免直接读 notes 造成数据竞争
	noteCount atomic.Int64

	seenStreamEnd int
	seenError     int
	seenToolCall  int

	// 渲染缓冲复用，避免每块分配
	monoBuf    []float32
	drumBuf    []float32
	drumOutBuf []float32
	outBuf     []float32
	duckGain   []float32

	// 本块内鼓点的起始位置（相对块首）。侧链据此压低旋律。
	duckOnsets []int

	// 待发的工具调用数量。事件到达时只累加，等到最近的网格点才发声——
	// 鼓点也必须遵守"数据控制密度、不控制位置"，否则节奏会跟着 token 走。
	pendingToolHits int
}

// maxPendingToolHits 限制积压：一次涌入太多工具调用时，只保留这么多次待发，
// 多的丢掉而不是排成长队慢慢敲——一拍一声的节奏下，排长队会让鼓点拖在事件后面。
const maxPendingToolHits = 3

// newEngine 构造引擎。seed 固定时输出可复现。
func newEngine(sampleRate, bpm, rootMIDI int, seed uint64) *Engine {
	engine := &Engine{
		sampleRate:   sampleRate,
		rootMIDI:     rootMIDI,
		rng:          rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		silenceAfter: silenceAfterBeats * 60.0 / float64(bpm),
		gridSamples:  max(1, int64(float64(sampleRate)*60.0/float64(bpm)/gridPerBeat)),
		notes:        make([]activeNote, 0, 64),
		reverb:       newReverb(sampleRate),
		drumReverb:   newReverbWith(sampleRate, drumDelayMsLeft, drumDelayMsRight, drumFeedback),
	}
	engine.state.Store(&SynthState{Phase: phaseIdle, IdleSec: math.Inf(1)})
	return engine
}

// SetState 整体替换状态。渲染路径读的是一次性引用，替换本身是原子的。
func (e *Engine) SetState(state SynthState) {
	e.state.Store(&state)
}

// ActiveNotes 返回当前正在发声的音符数，供可观测性输出用。
// 走 atomic 计数，可从状态行 goroutine 安全读取。
func (e *Engine) ActiveNotes() int { return int(e.noteCount.Load()) }

// State 返回当前状态快照，供可观测性输出用。
func (e *Engine) State() *SynthState { return e.state.Load() }

// Render 渲染 frames 个采样，返回 interleaved 立体声（长度 frames*2）。
// 返回的切片是引擎内部缓冲，调用方必须用完即弃，不要跨调用持有。
func (e *Engine) Render(frames int) []float32 {
	state := e.state.Load()

	e.ensureBuffers(frames)
	melody := e.monoBuf[:frames]
	drums := e.drumBuf[:frames]
	clear(melody)
	clear(drums)
	e.duckOnsets = e.duckOnsets[:0]

	e.schedule(state, frames)
	if state.StreamEndSeq > e.seenStreamEnd {
		e.seenStreamEnd = state.StreamEndSeq
		e.soundCadence(state)
	}
	if state.ErrorSeq > e.seenError {
		e.seenError = state.ErrorSeq
		e.soundError(state)
	}
	if state.ToolCallSeq > e.seenToolCall {
		// 只记待发数量，等网格点再发声
		e.pendingToolHits = min(e.pendingToolHits+(state.ToolCallSeq-e.seenToolCall), maxPendingToolHits)
		e.seenToolCall = state.ToolCallSeq
	}

	e.renderNotes(melody, drums, frames)
	e.sampleClock += int64(frames)
	e.pruneNotes()

	// 侧链：鼓点出现时把旋律压下去，给它让出空间
	e.applyDucking(melody, frames)
	for i := range melody {
		melody[i] *= melodyGain
	}

	// 旋律走长混响；鼓点走独立的短反射。
	// 共用长混响会把鼓的瞬态抹开（听感"在远处"），完全干声又"死"——
	// 真实混音里这两者本来就是分开的。
	wet := reverbWetBase + reverbWetGain*state.Energy
	out := e.outBuf[:frames*2]
	e.reverb.Process(melody, wet, out)

	drumOut := e.drumOutBuf[:frames*2]
	e.drumReverb.Process(drums, drumWet, drumOut)
	for i := range out {
		out[i] += drumOut[i]
	}

	for i := range out {
		out[i] = float32(math.Max(-maxAmplitude, math.Min(maxAmplitude, float64(out[i]))))
	}
	return out
}

// ensureBuffers 按需扩容复用缓冲。
func (e *Engine) ensureBuffers(frames int) {
	if cap(e.monoBuf) < frames {
		e.monoBuf = make([]float32, frames)
	}
	e.monoBuf = e.monoBuf[:frames]
	if cap(e.drumBuf) < frames {
		e.drumBuf = make([]float32, frames)
	}
	e.drumBuf = e.drumBuf[:frames]
	if cap(e.duckGain) < frames {
		e.duckGain = make([]float32, frames)
	}
	e.duckGain = e.duckGain[:frames]
	if cap(e.drumOutBuf) < frames*2 {
		e.drumOutBuf = make([]float32, frames*2)
	}
	e.drumOutBuf = e.drumOutBuf[:frames*2]
	if cap(e.outBuf) < frames*2 {
		e.outBuf = make([]float32, frames*2)
	}
	e.outBuf = e.outBuf[:frames*2]
}

// applyDucking 把本块的旋律乘以侧链增益曲线。
// 鼓点越近压得越深，之后指数恢复。
func (e *Engine) applyDucking(melody []float32, frames int) {
	if len(e.duckOnsets) == 0 {
		return
	}
	gain := e.duckGain[:frames]

	attack := max(1, int(duckAttackSec*float64(e.sampleRate)))
	recover := math.Max(1, duckRecoverSec*float64(e.sampleRate))

	for i := range gain {
		lowest := 1.0
		for _, onset := range e.duckOnsets {
			if i < onset {
				continue
			}
			relative := float64(i - onset)
			press := math.Min(1, relative/float64(attack))
			release := math.Exp(-math.Max(relative-float64(attack), 0) / recover)
			if value := 1 - (1-duckDepth)*press*release; value < lowest {
				lowest = value
			}
		}
		gain[i] = float32(lowest)
	}
	for i := range melody {
		melody[i] *= gain[i]
	}
}

// schedule 推进到本次渲染结束，途经的网格点逐个判定是否发声。
func (e *Engine) schedule(state *SynthState, frames int) {
	horizon := e.sampleClock + int64(frames)
	for e.nextGrid < horizon {
		offset := int(e.nextGrid - e.sampleClock)
		e.maybeTrigger(state, offset)
		e.nextGrid += e.gridSamples
		e.gridIndex++
	}
}

// maybeTrigger 在网格点上决定是否发声。位置固定，只有密度受数据影响。
func (e *Engine) maybeTrigger(state *SynthState, offset int) {
	// 长时间没有输入就彻底安静 —— 没有内容却一直响，听者分不清
	// "模型在思考"和"根本没在跑"，也会让长时间挂着听变成负担。
	if state.IdleSec >= e.silenceAfter {
		return
	}

	// 工具调用最先处理。它吸附到**拍**上而不是 16 分音符上——
	// 一拍最多一声。实测素材里有近一半的工具调用间隔小于一拍，
	// 落在 16 分音符网格上会变成"一拍两声"，听起来碎。
	// EDM 底鼓的标准密度本来就是每拍一下（four-on-the-floor）。
	if e.pendingToolHits > 0 && e.gridIndex%gridPerBeat == 0 {
		e.pendingToolHits--
		e.soundToolHit(state, offset)
	}

	spec := voiceFor(state.Phase)
	stride := phaseStride[state.Phase]
	if stride == 0 {
		stride = 2
	}

	if e.gridIndex%gridPerBar == 0 {
		// 固定骨架：每小节首拍必有一个轻和弦锚点，保证无论数据多稀疏音乐都成立
		e.addNote(spec, state, offset, downbeatVelocity, true)
		return
	}
	if e.gridIndex%int64(stride) != 0 {
		return
	}
	chance := baseTriggerChance + densityTriggerGain*state.Density
	if e.rng.Float64() >= chance {
		return
	}
	e.addNote(spec, state, offset, 1.0, false)
}

// addNote 把一个音符排进调度队列。
func (e *Engine) addNote(spec VoiceSpec, state *SynthState, offset int, velocityScale float64, isAnchor bool) {
	degree := e.pickDegree(spec, state, isAnchor)
	velocity := spec.BaseVelocity * velocityScale
	velocity *= 0.6 + 0.4*state.Accent
	velocity *= 0.75 + 0.25*state.Density

	duration := int(spec.DurationSec * float64(e.sampleRate))
	if duration <= 0 {
		return
	}
	e.notes = append(e.notes, activeNote{
		start:        int(e.sampleClock) + offset,
		duration:     duration,
		freq:         noteFreq(degree, e.rootMIDI),
		velocity:     math.Min(1, velocity),
		harmonics:    spec.Harmonics,
		attack:       int(spec.AttackSec * float64(e.sampleRate)),
		decay:        max(1, int(spec.DecaySec*float64(e.sampleRate))),
		isPercussive: spec.IsPercussive,
	})
}

// pickDegree 在音域内取音。锚点固定取最低音级，其余围绕 pitchBias 抖动。
func (e *Engine) pickDegree(spec VoiceSpec, state *SynthState, isAnchor bool) int {
	if isAnchor {
		return spec.DegreeLow
	}
	span := spec.DegreeHigh - spec.DegreeLow
	center := float64(spec.DegreeLow) + state.PitchBias*float64(span)
	jitter := e.rng.IntN(3) - 1
	degree := int(math.Round(center)) + jitter
	return min(spec.DegreeHigh, max(spec.DegreeLow, degree))
}

// soundCadence 一个流正常结束时奏终止式：主音 + 五度 + 八度，
// 和弦、上行、落回主音，给听者闭合感（CAITLAN 的 exit 动机）。
func (e *Engine) soundCadence(state *SynthState) {
	base := voiceFor(state.Phase).DegreeLow
	for _, degree := range []int{base, base + 3, base + 5} {
		e.notes = append(e.notes, activeNote{
			start:     int(e.sampleClock),
			duration:  int(2.4 * float64(e.sampleRate)),
			freq:      noteFreq(degree, e.rootMIDI),
			velocity:  0.3,
			harmonics: []float64{1.0, 0.2},
			attack:    int(0.02 * float64(e.sampleRate)),
			decay:     int(2.6 * float64(e.sampleRate)),
		})
	}
}

// soundToolHit 工具调用：无音高打击，叠加在任何相位之上。
//
// 它不走相位切换——流式下只有首个分片带 tool name，信号稀疏到连续命中不到迟滞
// 要求的次数，结果就是整个相位从不激活（实测真实素材里 4510 个工具调用增量，
// 一次都没触发过）。改成独立通道后每次调用都能听到。
func (e *Engine) soundToolHit(state *SynthState, offset int) {
	spec := voices[toolHit]
	// 侧链从鼓点实际落点开始，而不是块首
	e.duckOnsets = append(e.duckOnsets, offset)
	e.notes = append(e.notes, activeNote{
		start:        int(e.sampleClock) + offset,
		duration:     int(spec.DurationSec * float64(e.sampleRate)),
		freq:         noteFreq(spec.DegreeLow, e.rootMIDI),
		velocity:     spec.BaseVelocity * (0.6 + 0.4*state.Accent),
		harmonics:    spec.Harmonics,
		attack:       int(spec.AttackSec * float64(e.sampleRate)),
		decay:        max(1, int(spec.DecaySec*float64(e.sampleRate))),
		isPercussive: spec.IsPercussive,
		ducks:        true,
		pitchDrop:    spec.PitchDrop,
		dropTau:      spec.DropTauSec,
	})
}

// soundError 上游失败时奏错误动机。
//
// 必须与终止式**在多个维度上同时不同**才可能被听出来——只改一个音高是听不出的：
// 终止式是和弦、上行、落到主音、长音；错误动机是旋律、下行、停在二度、短促。
// 落点刻意避开主音与五度，留一个"没说完"的感觉。
func (e *Engine) soundError(state *SynthState) {
	base := voiceFor(state.Phase).DegreeLow
	// 五度 → 三度 → 二度，全程下行且不落到主音
	for index, degree := range []int{base + 3, base + 2, base + 1} {
		e.notes = append(e.notes, activeNote{
			start:     int(e.sampleClock) + int(float64(index)*errorNoteGapSec*float64(e.sampleRate)),
			duration:  int(errorNoteDurationSec * float64(e.sampleRate)),
			freq:      noteFreq(degree, e.rootMIDI),
			velocity:  0.32,
			harmonics: []float64{1.0, 0.25},
			attack:    int(0.008 * float64(e.sampleRate)),
			decay:     int(errorNoteDurationSec * float64(e.sampleRate)),
		})
	}
}

// renderNotes 把活跃音符叠加进缓冲。只渲染与本次窗口相交的部分。
//
// 走两条总线：鼓点单独一条（不受侧链压低），其余进旋律总线。
func (e *Engine) renderNotes(melody, drums []float32, frames int) {
	windowStart := int(e.sampleClock)
	windowEnd := windowStart + frames

	for i := range e.notes {
		note := &e.notes[i]
		low := max(note.start, windowStart)
		high := min(note.start+note.duration, windowEnd)
		if low >= high {
			continue
		}

		target := melody
		if note.ducks {
			target = drums
		}
		for idx := low; idx < high; idx++ {
			local := idx - note.start
			wave := e.waveform(note, local)
			target[idx-windowStart] += float32(wave * e.envelope(note, local) * note.velocity)
		}
	}
}

// waveform 生成单个样本。打击乐用高斯噪声，其余是谐波叠加。
func (e *Engine) waveform(note *activeNote, local int) float64 {
	if note.isPercussive {
		return e.rng.NormFloat64()
	}

	t := float64(local) / float64(e.sampleRate)
	phase := 2 * math.Pi * note.freq * t

	if note.pitchDrop > 0 {
		// 频率按 f(t) = freq·(1 + drop·e^(-t/tau)) 下滑，相位是它的积分：
		//   ∫f dt = freq·[t + drop·tau·(1 − e^(-t/tau))]
		// 解析求出即可，不必逐样本累加相位。
		phase = 2 * math.Pi * note.freq *
			(t + note.pitchDrop*note.dropTau*(1-math.Exp(-t/note.dropTau)))
	}

	wave := 0.0
	for index, amplitude := range note.harmonics {
		if amplitude == 0 {
			continue // 三角波/方波样式里有整位为零的谐波
		}
		// 谐波共用相位，所以泛音跟着一起下滑，保持整数倍关系
		wave += amplitude * math.Sin(float64(index+1)*phase)
	}
	return wave
}

// envelope 是起音、衰减、尾部淡出三段相乘。
func (e *Engine) envelope(note *activeNote, local int) float64 {
	attack := max(1, note.attack)
	rise := math.Min(1, float64(local)/float64(attack))
	pastAttack := math.Max(0, float64(local-attack))
	fall := math.Exp(-pastAttack / float64(note.decay))

	remaining := float64(note.duration - local)
	fadeLen := math.Max(1, tailFadeSec*float64(e.sampleRate))
	tail := math.Min(1, remaining/fadeLen)
	return rise * fall * tail
}

// pruneNotes 原地过滤已结束的音符。
// 原地前移而非重新分配——长时运行时这决定了切片底层数组不会无限增长。
func (e *Engine) pruneNotes() {
	alive := e.notes[:0]
	for _, note := range e.notes {
		if int64(note.start+note.duration) > e.sampleClock {
			alive = append(alive, note)
		}
	}
	e.notes = alive
	e.noteCount.Store(int64(len(alive)))
}
