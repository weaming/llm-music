package main

// 反馈延迟混响。不追求物理正确，只要给声音一点空间感——
// 干声在长时间聆听下会很快让人疲劳，而疲劳正是这类系统的头号失败模式。
// 左右声道用不同延迟，顺带得到一点立体声宽度。

const (
	delayMsLeft  = 73.0
	delayMsRight = 97.0
	feedback     = 0.55
	maxWet       = 0.45

	// 鼓用的短反射：延迟短、反馈低，只给一点"体"而不拉开距离
	drumDelayMsLeft  = 19.0
	drumDelayMsRight = 27.0
	drumFeedback     = 0.22
	drumWet          = 0.18
)

// Reverb 是双声道反馈延迟。
type Reverb struct {
	left, right       []float32
	leftIdx, rightIdx int
	feedback          float32
}

// newReverb 是旋律用的长混响。
func newReverb(sampleRate int) *Reverb {
	return newReverbWith(sampleRate, delayMsLeft, delayMsRight, feedback)
}

// newReverbWith 按给定延迟与反馈分配延迟线。
//
// 分两条的意义：真实混音里鼓用短房间、旋律用长混响。共用长混响会让鼓听起来
// "在远处"（空间把瞬态抹开了）；完全干声又会"死"。鼓要的是**短而低**的反射。
func newReverbWith(sampleRate int, delayLeft, delayRight, fb float64) *Reverb {
	leftLen := max(1, int(float64(sampleRate)*delayLeft/1000))
	rightLen := max(1, int(float64(sampleRate)*delayRight/1000))
	return &Reverb{
		left:     make([]float32, leftLen),
		right:    make([]float32, rightLen),
		feedback: float32(fb),
	}
}

// Process 把单声道信号变成加了混响的立体声，写进 dst。
// dst 是 interleaved 立体声，长度必须是 len(mono)*2。
// wet 是 0..1 的湿声比例。
func (r *Reverb) Process(mono []float32, wet float64, dst []float32) {
	if wet < 0 {
		wet = 0
	} else if wet > 1 {
		wet = 1
	}
	wetFactor := float32(wet * maxWet)
	dryFactor := 1 - wetFactor

	for i, sample := range mono {
		leftReflect := r.left[r.leftIdx]
		rightReflect := r.right[r.rightIdx]

		r.left[r.leftIdx] = sample + leftReflect*r.feedback
		r.right[r.rightIdx] = sample + rightReflect*r.feedback

		r.leftIdx++
		if r.leftIdx >= len(r.left) {
			r.leftIdx = 0
		}
		r.rightIdx++
		if r.rightIdx >= len(r.right) {
			r.rightIdx = 0
		}

		dst[i*2] = sample*dryFactor + leftReflect*wetFactor
		dst[i*2+1] = sample*dryFactor + rightReflect*wetFactor
	}
}

// Reset 清空延迟线。
func (r *Reverb) Reset() {
	clear(r.left)
	clear(r.right)
}
