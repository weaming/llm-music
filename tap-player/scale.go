package main

import "math"

// 大调五声：宫 商 角 徵 羽，相对主音的半音数。
// 选五声而非大小调，是为了从根上避免"意外语义"——任意两音都协和，
// 没有小二度与三全音，所以随机取音也不会难听。
var majorPentatonic = [...]int{0, 2, 4, 7, 9}

// 十二平均律基准：A4 = 440Hz，MIDI 69
const (
	a4MIDI = 69
	a4Freq = 440.0
)

// degreeToSemitone 把音级序号换算成相对主音的半音数。跨八度自动回绕。
func degreeToSemitone(degree int) int {
	octave, index := pythonDivMod(degree, len(majorPentatonic))
	return octave*12 + majorPentatonic[index]
}

// noteFreq 把音级序号换算成频率（Hz）。
func noteFreq(degree, rootMIDI int) float64 {
	midi := rootMIDI + degreeToSemitone(degree)
	return a4Freq * math.Pow(2.0, float64(midi-a4MIDI)/12.0)
}

// droneDegrees 固定和声层：主音 + 五度。
func droneDegrees(rootDegree int) (int, int) {
	return rootDegree, rootDegree + 3
}

// pythonDivMod 实现 Python 的 divmod 语义（余数恒为非负）。
//
// Go 的 / 与 % 对负数是截断向零的：-1/5 == 0、-1%5 == -1；
// Python 是向下取整：divmod(-1, 5) == (-1, 4)。音级序号会取到负数
// （低八度），所以必须用 Python 语义，否则低音区的音会算错一个八度。
func pythonDivMod(a, b int) (int, int) {
	quotient := a / b
	remainder := a % b
	if remainder < 0 {
		quotient--
		remainder += b
	}
	return quotient, remainder
}
