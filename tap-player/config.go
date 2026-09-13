package main

import (
	"fmt"
	"os"
	"strconv"
)

// 默认值与 Python 版逐字一致，两版行为才能对拍。
const (
	DefaultTapURL     = "http://127.0.0.1:3050/v1/tap"
	DefaultSampleRate = 48000
	DefaultBPM        = 120
	DefaultRootMIDI   = 62 // D4
	DefaultBlocksize  = 1024
	DefaultMaxGapSec  = 2.0
)

// Settings 是运行时配置。命令行参数优先于环境变量。
type Settings struct {
	TapURL     string
	SampleRate int
	BPM        int
	RootMIDI   int
	Blocksize  int
	MaxGap     float64
	Speed      float64
	Volume     float64
}

// defaultSettings 从环境变量构造配置。非法值打中文警告并回退默认。
func defaultSettings() Settings {
	return Settings{
		TapURL:     envString("TAP_URL", DefaultTapURL),
		SampleRate: envInt("TAP_SAMPLE_RATE", DefaultSampleRate),
		BPM:        envInt("TAP_BPM", DefaultBPM),
		RootMIDI:   envInt("TAP_ROOT_MIDI", DefaultRootMIDI),
		Blocksize:  envInt("TAP_BLOCKSIZE", DefaultBlocksize),
		MaxGap:     envFloat("TAP_MAX_GAP", DefaultMaxGapSec),
		Speed:      envFloat("TAP_SPEED", 1.0),
		Volume:     envFloat("TAP_VOLUME", 1.0),
	}
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s=%q 不是整数，回退 %d\n", name, value, fallback)
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s=%q 不是数字，回退 %g\n", name, value, fallback)
		return fallback
	}
	return parsed
}
