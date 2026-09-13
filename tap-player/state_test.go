package main

import (
	"math"
	"testing"
)

func feedMany(tracker *StateTracker, delta Delta, count int, start float64) {
	for i := range count {
		tracker.FeedDelta(delta, start+float64(i)*0.01)
	}
}

func TestTextDeltaClassifiedAsProse(t *testing.T) {
	tracker := newStateTracker()
	feedMany(tracker, Delta{Text: "你好"}, phaseHysteresis, 0)
	if got := tracker.Tick(1).Phase; got != phaseProse {
		t.Errorf("phase = %q, want %q", got, phaseProse)
	}
}

func TestReasoningTakesPriorityOverText(t *testing.T) {
	tracker := newStateTracker()
	feedMany(tracker, Delta{Text: "正文", Reasoning: "思考"}, phaseHysteresis, 0)
	if got := tracker.Tick(1).Phase; got != phaseReasoning {
		t.Errorf("phase = %q, want %q", got, phaseReasoning)
	}
}

func TestToolCallDoesNotChangePhase(t *testing.T) {
	// 工具调用是瞬时事件，不该切换相位——它走独立的打击通道。
	// 实测真实素材里 4510 个工具调用增量一次都没激活过相位：流式下只有首个
	// 分片带 name，稀疏到连续命中不到迟滞要求的次数。
	tracker := newStateTracker()
	feedMany(tracker, Delta{Text: "正文"}, phaseHysteresis, 0)
	feedMany(tracker, Delta{ToolName: "bash"}, phaseHysteresis*2, 1.0)

	state := tracker.Tick(2)
	if state.Phase != phaseProse {
		t.Errorf("相位不该被工具调用改变，得到 %q", state.Phase)
	}
	if state.ToolCallSeq != phaseHysteresis*2 {
		t.Errorf("ToolCallSeq = %d, want %d", state.ToolCallSeq, phaseHysteresis*2)
	}
}

func TestCodeFenceSwitchesPhase(t *testing.T) {
	tracker := newStateTracker()
	feedMany(tracker, Delta{Text: "说明"}, phaseHysteresis, 0)
	if got := tracker.Tick(1).Phase; got != phaseProse {
		t.Fatalf("起始应为 prose，得到 %q", got)
	}

	tracker.FeedDelta(Delta{Text: "```python"}, 1.0)
	feedMany(tracker, Delta{Text: "x = 1"}, phaseHysteresis, 1.1)
	if got := tracker.Tick(2).Phase; got != phaseCode {
		t.Errorf("围栏内应为 code，得到 %q", got)
	}

	tracker.FeedDelta(Delta{Text: "```"}, 2.0)
	feedMany(tracker, Delta{Text: "结尾"}, phaseHysteresis, 2.1)
	if got := tracker.Tick(3).Phase; got != phaseProse {
		t.Errorf("围栏闭合后应回到 prose，得到 %q", got)
	}
}

func TestCodeFenceSplitAcrossChunks(t *testing.T) {
	// ``` 被拆到相邻增量里也必须检出
	tracker := newStateTracker()
	tracker.FeedDelta(Delta{Text: "``"}, 0)
	tracker.FeedDelta(Delta{Text: "`py"}, 0.01)
	feedMany(tracker, Delta{Text: "code"}, phaseHysteresis, 0.02)
	if got := tracker.Tick(1).Phase; got != phaseCode {
		t.Errorf("拆分的三反引号应被检出，得到 %q", got)
	}
}

func TestPhaseSwitchNeedsHysteresis(t *testing.T) {
	// 单次噪声不该立刻切换相位
	tracker := newStateTracker()
	feedMany(tracker, Delta{Text: "正文"}, phaseHysteresis, 0)
	tracker.FeedDelta(Delta{Reasoning: "思考"}, 1.0)
	if got := tracker.Tick(1).Phase; got != phaseProse {
		t.Errorf("单次命中不该切换，得到 %q", got)
	}
	feedMany(tracker, Delta{Reasoning: "思考"}, phaseHysteresis, 1.01)
	if got := tracker.Tick(1.1).Phase; got != phaseReasoning {
		t.Errorf("连续命中后应切换，得到 %q", got)
	}
}

func TestDensityRisesWithArrivalRate(t *testing.T) {
	slow := newStateTracker()
	feedMany(slow, Delta{Text: "a"}, 2, 0)
	fast := newStateTracker()
	feedMany(fast, Delta{Text: "a"}, 40, 0)
	if fast.Tick(0.5).Density <= slow.Tick(0.5).Density {
		t.Error("到达越快密度应越高")
	}
}

func TestDensityDecaysWhenIdle(t *testing.T) {
	tracker := newStateTracker()
	feedMany(tracker, Delta{Text: "a"}, 100, 0)
	if tracker.Tick(0).Density <= 0.5 {
		t.Error("密集到达时密度应超过 0.5")
	}
	if got := tracker.Tick(10).Density; got != 0 {
		t.Errorf("空闲 10 秒后密度应归零，得到 %v", got)
	}
}

func TestEnergyHasVariation(t *testing.T) {
	// 1/f 包络代理：能量必须有起伏，不能是常数。
	// 时间跨度要覆盖能量时间常数（15 秒），否则看不出慢变量的运动。
	tracker := newStateTracker()
	var samples []float64
	now := 0.0

	for i := range 400 { // 8 秒高密度
		tracker.FeedDelta(Delta{Text: "a"}, now)
		now += 0.02
		if i%20 == 0 {
			samples = append(samples, tracker.Tick(now).Energy)
		}
	}
	for range 40 { // 40 秒静默
		now += 1
		samples = append(samples, tracker.Tick(now).Energy)
	}

	low, high := math.Inf(1), math.Inf(-1)
	for _, value := range samples {
		low = math.Min(low, value)
		high = math.Max(high, value)
	}
	if high-low <= 0.1 {
		t.Errorf("能量起伏过小: %.4f", high-low)
	}
}

func TestAccentFromPunctuation(t *testing.T) {
	strong := newStateTracker()
	if got := strong.FeedDelta(Delta{Text: "你好。"}, 0).Accent; got != accentStrong {
		t.Errorf("句号应触发强重音，得到 %v", got)
	}
	weak := newStateTracker()
	if got := weak.FeedDelta(Delta{Text: "你好，"}, 0).Accent; got != accentWeak {
		t.Errorf("逗号应触发弱重音，得到 %v", got)
	}
}

func TestAccentHoldsThenDecays(t *testing.T) {
	tracker := newStateTracker()
	tracker.FeedDelta(Delta{Text: "。"}, 0)
	if got := tracker.Tick(0).Accent; got != accentStrong {
		t.Errorf("刚设置时重音应为满值，得到 %v", got)
	}
	if got := tracker.Tick(0.3).Accent; got <= 0 || got >= accentStrong {
		t.Errorf("保持期内应处于衰减中，得到 %v", got)
	}
	if got := tracker.Tick(1).Accent; got != 0 {
		t.Errorf("超过保持时长应归零，得到 %v", got)
	}
}

func TestIdleSecondsIsInfiniteBeforeAnyContent(t *testing.T) {
	// 从未有过内容时应当直接静默，而不是先响一会儿
	if got := newStateTracker().Tick(100).IdleSec; !math.IsInf(got, 1) {
		t.Errorf("无内容时 IdleSec 应为 +Inf，得到 %v", got)
	}
}

func TestIdleSecondsTracksContent(t *testing.T) {
	tracker := newStateTracker()
	tracker.FeedDelta(Delta{Text: "a"}, 10)
	if got := tracker.Tick(12).IdleSec; math.Abs(got-2) > 1e-9 {
		t.Errorf("IdleSec = %v, want 2", got)
	}
}

func TestStreamEndAndErrorCountedSeparately(t *testing.T) {
	// 两者混在一起的话引擎会奏终止式而不是错误动机
	tracker := newStateTracker()
	state := tracker.FeedLifecycle(kindError, 0.1)
	if state.ErrorSeq != 1 || state.StreamEndSeq != 0 {
		t.Errorf("error 应独立计数，得到 error=%d stream_end=%d", state.ErrorSeq, state.StreamEndSeq)
	}
}
