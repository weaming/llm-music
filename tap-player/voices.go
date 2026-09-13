package main

// 动机族。依据 CAITLAN 的层次化主导动机设计：
// 异类靠动机族区分，同类靠节奏区分，且全部取自同一个五声音阶。
//
// 区分靠**两个维度同时不同**，缺一不可：
//   - 音品（谐波结构）：推理取三角波样式（暗）、正文近乎纯正弦（中性）、
//     代码取方波样式（亮）。只改音区是不够的——那听起来只是"同一件乐器换个音高"。
//   - 音区与时值：见各 VoiceSpec。
//
// 工具调用**不是相位**，而是叠加在任何相位之上的独立打击通道（toolHit）——
// 它是瞬时事件而非持续状态，走相位切换会被迟滞吃掉。
//
// 音域用音级序号表示，0 是主音，负数是低八度。
type VoiceSpec struct {
	DegreeLow    int
	DegreeHigh   int
	DurationSec  float64
	IsPercussive bool
	BaseVelocity float64
	// 泛音幅度，第一项是基频。低音区少用泛音以免浑浊。
	Harmonics []float64
	// 起音时长（秒）。太短会有爆音，太长会糊。
	AttackSec float64
	// 衰减时间常数（秒）。
	DecaySec float64
	// PitchDrop 是起音瞬间的频率倍数（0 = 不滑音）。
	// 真实鼓的膜张力在击打瞬间最高，频率从一个高值快速滑到基频——
	// **这个下滑才是"punch"的来源**，固定频率只会"嗡"不会"咚"。
	PitchDrop float64
	// DropTauSec 是下滑的时间常数（秒）。
	DropTauSec float64
}

const (
	phaseIdle      = "idle"
	phaseReasoning = "reasoning"
	phaseProse     = "prose"
	phaseCode      = "code"
	// 工具调用不是相位（它是瞬时事件而非持续状态），
	// 由 SynthState.ToolCallSeq 这个独立通道触发。
	toolHit = "tool_hit"
)

// downbeatVelocity 是每小节首拍那个轻和弦锚点的强度。
const downbeatVelocity = 0.3

var voices = map[string]VoiceSpec{
	// 静默：极稀疏的低音长音
	phaseIdle: {
		DegreeLow: -7, DegreeHigh: -3,
		DurationSec: 3.0, BaseVelocity: 0.25,
		Harmonics: []float64{1.0, 0.1},
		AttackSec: 0.25, DecaySec: 3.0,
	},
	// 推理：低音铺底 + 长时值。谐波取**三角波样式**（奇次、按 1/n² 快速衰减）
	// —— 听感"暗而柔"，与正文的近正弦形成音品差异，而不只是音区差异。
	phaseReasoning: {
		DegreeLow: -7, DegreeHigh: 2,
		DurationSec: 2.0, BaseVelocity: 0.35,
		Harmonics: []float64{1.0, 0, 0.11, 0, 0.04},
		AttackSec: 0.08, DecaySec: 2.2,
	},
	// 正文：中音区连贯旋律，主声部。近乎纯正弦，中性。
	phaseProse: {
		DegreeLow: 0, DegreeHigh: 7,
		DurationSec: 0.9, BaseVelocity: 0.5,
		Harmonics: []float64{1.0, 0.25, 0.08},
		AttackSec: 0.015, DecaySec: 1.0,
	},
	// 代码：断奏。谐波取**方波样式**（奇次、按 1/n 缓慢衰减）—— 听感"亮而有棱角"。
	phaseCode: {
		DegreeLow: 0, DegreeHigh: 5,
		DurationSec: 0.22, BaseVelocity: 0.45,
		Harmonics: []float64{1.0, 0, 0.33, 0, 0.2, 0, 0.14},
		AttackSec: 0.003, DecaySec: 0.16,
	},
	// 工具调用：低音鼓点。
	//
	// 音色上踩过四次坑，都记下来免得再犯：
	//  1. **不能用噪声**。白噪声是宽带的，和五声音阶的和谐旋律天然冲突，
	//     密集时听起来像放鞭炮。
	//  2. **不能停在 -7**。那是主音下方一个八度，既落在 reasoning 的音域
	//     （-7..2）里，又正是每小节首拍锚点取的音——同音同区，听感上只是
	//     "旋律的一部分"，不构成独立事件。
	//  3. **不能取高音区**。频段独立了，但高音短音像敲铁片，不是鼓。
	//  4. **不能没有音高下滑**。固定频率的短音只有"嗡"，没有鼓的"咚"——
	//     真实鼓的膜张力在击打瞬间最高，频率会从高值快速滑到基频。
	//
	// 现在：基频 -10（主音下方两个八度 ≈ 73Hz，标准底鼓频段），
	// 起音瞬间高一个八度（对应 -7 的音高，仍落在五声音阶内），30ms 内滑完。
	// 下滑一个八度既给出 punch，又和旋律同属一个调。
	//
	// **谐波不能省**：73Hz 基频在电脑音箱上多半放不出来，全靠谐波让人耳
	// "脑补"出低频。
	//
	// 它不作为相位使用，见上方说明。
	toolHit: {
		DegreeLow: -10, DegreeHigh: -10,
		// 时值与衰减都拉长：气势来自"轰"的尾巴，不只是击打那一下。
		DurationSec: 0.42, BaseVelocity: 1.0,
		// 能量集中在基频。真实底鼓的谐波比这还少，但 73Hz 在电脑音箱上
		// 放不出来，2 次谐波是"能听见"的最低保险。
		Harmonics: []float64{1.0, 0.45, 0.18},
		AttackSec: 0.002, DecaySec: 0.22,
		// 下滑深一点（1.2 个八度）冲击感更强
		PitchDrop: 1.2, DropTauSec: 0.04,
	},
}

// voiceFor 取相位对应的动机族；未知相位回退到正文。
func voiceFor(phase string) VoiceSpec {
	if spec, ok := voices[phase]; ok {
		return spec
	}
	return voices[phaseProse]
}
