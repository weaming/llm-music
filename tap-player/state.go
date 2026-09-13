package main

import (
	"math"
	"strings"
	"sync/atomic"
)

// 事件 → 合成状态的映射。这一层是"数据"与"声音"之间的全部接口：
// 事件只写状态，**从不直接触发声音**。引擎按自己的时钟读状态决定怎么发声——
// 这把 LLM token 的突发性隔离在音乐时钟之外。
//
// SynthState 是不可变快照，由事件线程整体替换、渲染路径读一次引用
// （Go 版用 atomic.Pointer 显式表达 Python 版靠 GIL 保证的那件事）。

const (
	// 密度滑窗：统计最近这段时间内到达的增量数
	densityWindowSec = 2.0
	// 滑窗内达到该速率即认为密度饱和
	densitySaturationRate = 40.0
	// 能量的慢速平滑时间常数——1/f 包络的代理，让长时段听感有起伏
	energyTauSec = 15.0
	// 相位切换迟滞：候选相位需连续命中这么多次才真正切换，避免抖动
	phaseHysteresis = 3
	// 重音保持时长：标点标记的是"接下来这一小段"的重音，不是单个瞬间。
	// 引擎在自己的网格点读状态，瞬间值会被完全错过。
	accentHoldSec = 0.6

	accentStrong  = 1.0
	accentWeak    = 0.6
	accentNewline = 0.8
)

const (
	strongPunct = "。！？!?"
	weakPunct   = ",;:，；：、"
)

// SynthState 是渲染引擎读到的全部数据侧信息。不可变，整体替换。
type SynthState struct {
	Phase        string
	Density      float64
	Energy       float64
	Accent       float64
	PitchBias    float64
	StreamEndSeq int
	ErrorSeq     int
	// IdleSec 是距上次内容事件的时间。从未有过内容时为 +Inf。
	IdleSec float64
	// ToolCallSeq 是工具调用的累计次数。它不参与相位判定，而是引擎的独立打击通道——
	// 流式下只有首个分片带 tool name，信号太稀疏，走相位迟滞会被整个吃掉。
	ToolCallSeq int
}

// StateTracker 把连续的增量流累积成 SynthState。
// 只在音频线程被访问，因此不需要锁。
type StateTracker struct {
	arrivals      []float64
	energy        float64
	lastTick      float64
	phase         string
	candidate     string
	candidateHits int
	inCodeFence   bool
	fenceTail     string
	accent        float64
	accentSetAt   float64
	pitchPhase    float64
	streamEndSeq  int
	errorSeq      int
	toolCallSeq   int
	tokens        atomic.Int64
	lastContentAt float64
	hasContent    bool
}

// newStateTracker 返回一个初始（静默）的跟踪器。
func newStateTracker() *StateTracker {
	return &StateTracker{
		phase:     phaseIdle,
		candidate: phaseIdle,
		// 上限：滑窗内最多这么多增量，再多也饱和了
		arrivals: make([]float64, 0, int(densityWindowSec*densitySaturationRate)+8),
	}
}

// Tokens 返回累计处理过的增量数，供可观测性输出使用。
// 走 atomic：状态行 goroutine 会在音频线程之外读它。
func (t *StateTracker) Tokens() int { return int(t.tokens.Load()) }

// FeedDelta 吃进一个内容增量。
func (t *StateTracker) FeedDelta(delta Delta, now float64) SynthState {
	t.arrivals = append(t.arrivals, now)
	t.tokens.Add(1)
	t.lastContentAt = now
	t.hasContent = true
	if delta.ToolName != "" {
		t.toolCallSeq++
	}
	t.updateFence(delta.Text)
	t.updateAccent(delta, now)
	t.advancePhase(t.classify(delta))
	return t.snapshot(now, true)
}

// FeedLifecycle 吃进一个生命周期事件。只有流结束或失败会改变状态。
func (t *StateTracker) FeedLifecycle(kind string, now float64) SynthState {
	switch kind {
	case kindStreamEnd:
		t.streamEndSeq++
	case kindError:
		t.errorSeq++
	}
	return t.snapshot(now, false)
}

// Tick 在没有新事件时推进——能量需要随时间衰减。
func (t *StateTracker) Tick(now float64) SynthState {
	return t.snapshot(now, false)
}

// classify 判定该增量属于哪个相位。reasoning 优先于正文。
//
// 工具调用不在这里判定——它是瞬时事件，走独立通道（见 SynthState.ToolCallSeq）。
func (t *StateTracker) classify(delta Delta) string {
	switch {
	case delta.Reasoning != "":
		return phaseReasoning
	case delta.Text != "":
		if t.inCodeFence {
			return phaseCode
		}
		return phaseProse
	default:
		return t.phase
	}
}

// advancePhase 带迟滞的相位切换：候选需连续命中若干次才生效。
func (t *StateTracker) advancePhase(target string) {
	if target == t.phase {
		t.candidateHits = 0
		return
	}
	if target == t.candidate {
		t.candidateHits++
	} else {
		t.candidate = target
		t.candidateHits = 1
	}
	if t.candidateHits >= phaseHysteresis {
		t.phase = target
		t.candidateHits = 0
	}
}

// updateFence 跟踪 markdown 代码围栏。
// 保留两字符尾巴，防止 ``` 被拆到相邻增量里漏检。
func (t *StateTracker) updateFence(text string) {
	if text == "" {
		return
	}
	t.fenceTail += text
	for {
		idx := strings.Index(t.fenceTail, "```")
		if idx < 0 {
			break
		}
		t.inCodeFence = !t.inCodeFence
		t.fenceTail = t.fenceTail[:idx] + t.fenceTail[idx+3:]
	}
	if len(t.fenceTail) > 2 {
		t.fenceTail = t.fenceTail[len(t.fenceTail)-2:]
	}
}

// updateAccent 标点与换行决定接下来一小段的重音强度。
func (t *StateTracker) updateAccent(delta Delta, now float64) {
	text := delta.Text
	if text == "" {
		text = delta.Reasoning
	}
	if text == "" {
		return
	}

	value := 0.0
	switch {
	case strings.ContainsAny(text, strongPunct):
		value = accentStrong
	case strings.Contains(text, "\n"):
		value = accentNewline
	case strings.ContainsAny(text, weakPunct):
		value = accentWeak
	}
	if value > t.accent {
		t.accent = value
		t.accentSetAt = now
	}
}

// currentAccent 重音随保持时长线性衰减到零。
func (t *StateTracker) currentAccent(now float64) float64 {
	if t.accent <= 0 {
		return 0
	}
	elapsed := now - t.accentSetAt
	if elapsed < 0 || elapsed >= accentHoldSec {
		return 0
	}
	return t.accent * (1 - elapsed/accentHoldSec)
}

// snapshot 产出一份状态快照，并推进所有随时间衰减的量。
func (t *StateTracker) snapshot(now float64, advancePitch bool) SynthState {
	t.decayArrivals(now)
	t.decayEnergy(now)
	if advancePitch {
		// 旋律在音阶上缓慢上下，避免单调爬升
		t.pitchPhase += 0.02 * (1 + t.density())
	}
	t.lastTick = now

	return SynthState{
		Phase:        t.phase,
		Density:      t.density(),
		Energy:       t.energy,
		Accent:       t.currentAccent(now),
		PitchBias:    0.5 + 0.5*math.Sin(t.pitchPhase),
		StreamEndSeq: t.streamEndSeq,
		ErrorSeq:     t.errorSeq,
		IdleSec:      t.idleSeconds(now),
		ToolCallSeq:  t.toolCallSeq,
	}
}

// idleSeconds 距上次内容事件多久。从未有过内容时返回 +Inf——
// 启动即静默，不是先响一会儿。
func (t *StateTracker) idleSeconds(now float64) float64 {
	if !t.hasContent {
		return math.Inf(1)
	}
	if now <= t.lastContentAt {
		return 0
	}
	return now - t.lastContentAt
}

// decayArrivals 丢掉滑窗之外的到达记录。
func (t *StateTracker) decayArrivals(now float64) {
	cutoff := now - densityWindowSec
	keep := 0
	for keep < len(t.arrivals) && t.arrivals[keep] < cutoff {
		keep++
	}
	if keep == 0 {
		return
	}
	// 原地前移，避免反复重新分配底层数组
	copy(t.arrivals, t.arrivals[keep:])
	t.arrivals = t.arrivals[:len(t.arrivals)-keep]
}

// density 滑窗内到达速率归一化到 0..1。
func (t *StateTracker) density() float64 {
	rate := float64(len(t.arrivals)) / densityWindowSec
	return math.Min(1, rate/densitySaturationRate)
}

// decayEnergy 指数平滑：快起慢落，形成长时段的能量起伏。
func (t *StateTracker) decayEnergy(now float64) {
	elapsed := now - t.lastTick
	if elapsed <= 0 {
		return
	}
	alpha := 1 - math.Exp(-elapsed/energyTauSec)
	t.energy += alpha * (t.density() - t.energy)
}
