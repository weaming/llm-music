package main

import (
	"math"
	"testing"
)

func TestPythonDivModMatchesPythonSemantics(t *testing.T) {
	// 这是移植时最容易搞错的一处：Go 的 / 与 % 对负数是截断向零的，
	// Python 的 divmod 是向下取整。音级序号会取到负数（低八度），
	// 用错语义会让低音区的音差一个八度。
	cases := []struct{ a, b, wantQ, wantR int }{
		{7, 5, 1, 2},
		{5, 5, 1, 0},
		{0, 5, 0, 0},
		{-1, 5, -1, 4}, // Python: divmod(-1, 5) == (-1, 4)
		{-5, 5, -1, 0},
		{-6, 5, -2, 4},
	}
	for _, c := range cases {
		q, r := pythonDivMod(c.a, c.b)
		if q != c.wantQ || r != c.wantR {
			t.Errorf("pythonDivMod(%d, %d) = (%d, %d), want (%d, %d)",
				c.a, c.b, q, r, c.wantQ, c.wantR)
		}
	}
}

func TestDegreeAscendsWithinOctave(t *testing.T) {
	for i, want := range majorPentatonic {
		if got := degreeToSemitone(i); got != want {
			t.Errorf("degreeToSemitone(%d) = %d, want %d", i, got, want)
		}
	}
}

func TestDegreeWrapsToNextOctave(t *testing.T) {
	if got := degreeToSemitone(len(majorPentatonic)); got != 12 {
		t.Errorf("跨八度应加 12，得到 %d", got)
	}
	// 负音级要落到低八度：Python 的 divmod(-1, 5) == (-1, 4) → -12 + 9 = -3
	if got := degreeToSemitone(-1); got != -3 {
		t.Errorf("degreeToSemitone(-1) = %d, want -3", got)
	}
}

func TestFrequencyDoublesAcrossOctave(t *testing.T) {
	base := noteFreq(0, DefaultRootMIDI)
	upper := noteFreq(len(majorPentatonic), DefaultRootMIDI)
	if math.Abs(upper/base-2.0) > 1e-9 {
		t.Errorf("跨八度应恰好翻倍，比值 %v", upper/base)
	}
}

func TestRootNoteMatchesMIDI(t *testing.T) {
	// 主音 D4 = MIDI 62 ≈ 293.66Hz
	if got := noteFreq(0, 62); math.Abs(got-293.6648) > 0.01 {
		t.Errorf("D4 = %v Hz, want ≈293.66", got)
	}
}

func TestDroneIsRootAndFifth(t *testing.T) {
	root, fifth := droneDegrees(0)
	if degreeToSemitone(fifth)-degreeToSemitone(root) != 7 {
		t.Errorf("drone 应为纯五度，得到 %d 个半音",
			degreeToSemitone(fifth)-degreeToSemitone(root))
	}
}

func TestPentatonicHasNoSemitoneOrTritone(t *testing.T) {
	// 五声的关键性质：任意两音之间既无小二度也无三全音，随机取音不会难听。
	intervals := map[int]bool{}
	for _, a := range majorPentatonic {
		for _, b := range majorPentatonic {
			diff := a - b
			if diff < 0 {
				diff = -diff
			}
			intervals[diff%12] = true
		}
	}
	for _, banned := range []int{1, 11, 6} {
		if intervals[banned] {
			t.Errorf("五声音阶不该出现 %d 个半音的间隔", banned)
		}
	}
}
