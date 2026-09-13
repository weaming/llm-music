package main

import (
	"math"
	"testing"
)

const testSampleRate = 48000

// activeState 构造一个"刚有输入"的状态。
// SynthState 的 IdleSec 默认是 0 值（= 刚有内容），但为了表意清晰，
// 想让引擎发声的测试都用这个助手，需要测静默的用例自行覆盖 IdleSec。
func activeState() SynthState {
	return SynthState{Phase: phaseProse, IdleSec: 0}
}

// renderSeconds 离线渲染指定时长，返回 interleaved 立体声。
func renderSeconds(engine *Engine, seconds float64, block int) []float32 {
	total := int(seconds * float64(engine.sampleRate))
	out := make([]float32, 0, total*2)
	for remaining := total; remaining > 0; {
		frames := min(block, remaining)
		out = append(out, engine.Render(frames)...)
		remaining -= frames
	}
	return out
}

// rms 计算整段能量。
func rms(audio []float32) float64 {
	if len(audio) == 0 {
		return 0
	}
	sum := 0.0
	for _, sample := range audio {
		sum += float64(sample) * float64(sample)
	}
	return math.Sqrt(sum / float64(len(audio)))
}

// zeroCrossingRate 是"亮度"的粗略代理：音越高过零越多。
// 比手写 DFT 简单，且足以表达"动机族可区分"。
func zeroCrossingRate(audio []float32) float64 {
	if len(audio) < 2 {
		return 0
	}
	crossings := 0
	previous := audio[0]
	for i := 2; i < len(audio); i += 2 { // 只看左声道
		if (previous < 0) != (audio[i] < 0) {
			crossings++
		}
		previous = audio[i]
	}
	return float64(crossings) / float64(len(audio)/2)
}

// noteFreqs 取出引擎当前排期的音符频率，供动机断言使用。
func noteFreqs(engine *Engine) []float64 {
	freqs := make([]float64, 0, len(engine.notes))
	for _, note := range engine.notes {
		freqs = append(freqs, note.freq)
	}
	return freqs
}

func TestEngineSilentWithoutInput(t *testing.T) {
	// 没有输入就不该发声——静默才是默认状态。
	// 一直响会让人分不清"模型在思考"和"根本没在跑"。
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	audio := renderSeconds(engine, 5, 1024)
	if rms(audio) != 0 {
		t.Errorf("无输入时应完全静默，RMS=%v", rms(audio))
	}
}

func TestEngineSoundsWithRecentInput(t *testing.T) {
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	engine.SetState(activeState())
	if rms(renderSeconds(engine, 6, 1024)) <= 0 {
		t.Error("有近期输入时应当发声")
	}
}

func TestEngineGoesSilentAfterInputStops(t *testing.T) {
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	silenceAfter := engine.silenceAfter
	if silenceAfter <= 0 {
		t.Fatal("静默阈值应大于 0")
	}

	// 阈值之内还该有声
	engine.SetState(activeState())
	if rms(renderSeconds(engine, 1, 1024)) <= 0 {
		t.Error("阈值内应当仍有声音")
	}

	// 越过阈值（留足已有音符衰减的时间）之后应收敛到静音。
	// 用阈值而非 == 0：混响的反馈环会留下极小的浮点残差。
	state := activeState()
	state.IdleSec = silenceAfter + 0.1
	engine.SetState(state)
	tail := renderSeconds(engine, 4, 1024)
	lastSecond := tail[len(tail)-testSampleRate*2:]
	if rms(lastSecond) > 1e-6 {
		t.Errorf("越过静默阈值后应收敛到静音，RMS=%v", rms(lastSecond))
	}
}

func TestOutputNeverClips(t *testing.T) {
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	state := activeState()
	state.Phase = phaseCode
	state.Density = 1
	state.Energy = 1
	state.Accent = 1
	engine.SetState(state)

	for _, sample := range renderSeconds(engine, 6, 1024) {
		if math.IsNaN(float64(sample)) || math.Abs(float64(sample)) > 1 {
			t.Fatalf("输出越界: %v", sample)
		}
	}
}

func TestPhasesAreDistinguishable(t *testing.T) {
	// 动机族必须可辨识——这是 CAITLAN 设计的核心要求。
	// reasoning 在低音区、code 在中音区，过零率应当明显不同。
	low := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 1)
	lowState := activeState()
	lowState.Phase = phaseReasoning
	lowState.Density = 0.8
	low.SetState(lowState)

	high := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 1)
	highState := activeState()
	highState.Phase = phaseCode
	highState.Density = 0.8
	high.SetState(highState)

	lowRate := zeroCrossingRate(renderSeconds(low, 6, 1024))
	highRate := zeroCrossingRate(renderSeconds(high, 6, 1024))
	if highRate <= lowRate*1.2 {
		t.Errorf("code 应比 reasoning 明显更亮: low=%v high=%v", lowRate, highRate)
	}
}

func TestCadenceAscendsToTonic(t *testing.T) {
	// 终止式上行并落到主音——这是"解决"的来源
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	engine.soundCadence(&SynthState{Phase: phaseProse})
	freqs := noteFreqs(engine)

	if len(freqs) != 3 {
		t.Fatalf("终止式应有 3 个音，得到 %d", len(freqs))
	}
	for i := 1; i < len(freqs); i++ {
		if freqs[i] <= freqs[i-1] {
			t.Errorf("终止式应上行，得到 %v", freqs)
		}
	}
	tonic := noteFreq(0, DefaultRootMIDI)
	if math.Abs(freqs[0]-tonic) > 1e-9 {
		t.Errorf("终止式应落在主音 %v，得到 %v", tonic, freqs[0])
	}
}

func TestErrorMotiveDescendsAndAvoidsTonic(t *testing.T) {
	// 错误动机必须与终止式可分辨：下行、且刻意不落到主音。
	// 只改一个音高是听不出来的，所以它在形态、时值、方向、落点四个维度同时不同。
	// 这个测试守住方向与落点这两个最容易在重构中被无意改掉的属性。
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	engine.soundError(&SynthState{Phase: phaseProse})
	freqs := noteFreqs(engine)

	if len(freqs) != 3 {
		t.Fatalf("错误动机应有 3 个音，得到 %d", len(freqs))
	}
	for i := 1; i < len(freqs); i++ {
		if freqs[i] >= freqs[i-1] {
			t.Errorf("错误动机应下行，得到 %v", freqs)
		}
	}
	tonic := noteFreq(0, DefaultRootMIDI)
	for _, freq := range freqs {
		if math.Abs(freq-tonic) < 1e-9 {
			t.Errorf("错误动机不该落到主音，得到 %v", freqs)
		}
	}
}

func TestCadenceAndErrorProduceDifferentAudio(t *testing.T) {
	// 回归：错误不该复用终止式
	cadence := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 11)
	cadenceState := activeState()
	cadenceState.StreamEndSeq = 1
	cadence.SetState(cadenceState)

	errorEngine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 11)
	errorState := activeState()
	errorState.ErrorSeq = 1
	errorEngine.SetState(errorState)

	cadenceAudio := renderSeconds(cadence, 3, 1024)
	errorAudio := renderSeconds(errorEngine, 3, 1024)
	if len(cadenceAudio) != len(errorAudio) {
		t.Fatal("两段长度应一致")
	}
	identical := true
	for i := range cadenceAudio {
		if math.Abs(float64(cadenceAudio[i]-errorAudio[i])) > 1e-9 {
			identical = false
			break
		}
	}
	if identical {
		t.Error("错误动机与终止式不该产生完全相同的音频")
	}
}

func TestToolHitDescendsInPitch(t *testing.T) {
	// 真实鼓的膜张力在击打瞬间最高，频率从一个高值快速滑到基频——
	// 这个下滑才是"咚"的来源。固定频率只会"嗡"。
	//
	// 用**过零间隔**测瞬时频率：零交叉率在短窗口里不可靠（基频周期就有 13.7ms，
	// 窗口比一个周期还短时计数毫无意义），而间隔能逐周期反映当前频率。
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	// 用**静默状态**隔离鼓点：IdleSec 超阈值时引擎不调度旋律，
	// 但工具打击走独立通道不受影响。否则测到的是混音，过零全来自旋律。
	silent := SynthState{Phase: phaseProse, IdleSec: math.Inf(1)}
	engine.SetState(silent)
	engine.soundToolHit(&silent, 0)

	spec := voices[toolHit]
	if spec.PitchDrop <= 0 {
		t.Fatal("鼓点应配置音高下滑")
	}

	out := engine.Render(testSampleRate)
	mono := make([]float64, len(out)/2)
	for i := range mono {
		mono[i] = float64(out[i*2])
	}

	// 用**过零率（计数÷时长）**而非过零间隔。
	// 多谐波波形里间隔换算不出基频（一个基频周期内会多次过零），
	// 但过零率与基频成正比——波形形状不变时，**两者之比就是频率之比**。
	zcr := func(start, length int) float64 {
		crossings := 0
		for i := start + 1; i < start+length && i < len(mono); i++ {
			if (mono[i-1] < 0) != (mono[i] < 0) {
				crossings++
			}
		}
		return float64(crossings) / (float64(length) / float64(testSampleRate))
	}

	// 起音段取前 30ms；稳定段取 100–130ms（此时已滑完，且衰减到 37% 仍可测）
	const seg = testSampleRate * 30 / 1000
	onset := zcr(0, seg)
	steady := zcr(testSampleRate/10, seg)

	if onset <= steady*1.3 {
		t.Errorf("起音频率应明显高于稳定段：onset=%.0fHz steady=%.0fHz", onset, steady)
	}
	base := noteFreq(spec.DegreeLow, DefaultRootMIDI)
	t.Logf("过零率 起音 %.0f → 稳定 %.0f（比值 %.2f，理论 %.2f）",
		onset, steady, onset/steady, 1+spec.PitchDrop)
	_ = base
}

func TestDeterministicWithSameSeed(t *testing.T) {
	first := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 42)
	first.SetState(activeState())
	second := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 42)
	second.SetState(activeState())

	a := renderSeconds(first, 2, 1024)
	b := renderSeconds(second, 2, 1024)
	if len(a) != len(b) {
		t.Fatal("长度应一致")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("同种子应完全可复现，第 %d 个样本不同", i)
		}
	}
}

func TestNotesArePruned(t *testing.T) {
	// 长时运行的关键：已结束的音符必须被清理，否则切片会无限增长
	engine := newEngine(testSampleRate, DefaultBPM, DefaultRootMIDI, 0)
	engine.SetState(activeState())
	renderSeconds(engine, 30, 1024)

	// 30 秒后活跃音符数应当是有界的（远小于累计触发数）
	if engine.ActiveNotes() > 200 {
		t.Errorf("音符未被清理，活跃数 %d", engine.ActiveNotes())
	}
}

func TestVariousTemposRender(t *testing.T) {
	for _, bpm := range []int{60, 120, 180} {
		engine := newEngine(testSampleRate, bpm, DefaultRootMIDI, 0)
		engine.SetState(activeState())
		if rms(renderSeconds(engine, 4, 1024)) <= 0 {
			t.Errorf("bpm=%d 时应能渲出声音", bpm)
		}
	}
}
