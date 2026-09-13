package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// 把 tap 事件原样落成 jsonl，供之后重放调音色。
// 格式与 Python 版完全一致，两版的素材可以互换使用。

// Recorder 追加写 jsonl。每条都 flush，方便边录边看。
type Recorder struct {
	file        *os.File
	writer      *bufio.Writer
	count       int
	preExisting int64
}

// newRecorder 打开（或创建）录制文件。
// **追加而非覆盖**：多次录制累积到同一份素材里。已有内容时调用方应提示一声，
// 否则重放时会连带重放上一次录的，容易让人以为程序出了错。
func newRecorder(path string) (*Recorder, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建录制目录失败: %w", err)
		}
	}

	var preExisting int64
	if info, err := os.Stat(path); err == nil {
		preExisting = info.Size()
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开录制文件失败: %w", err)
	}
	return &Recorder{
		file:        file,
		writer:      bufio.NewWriter(file),
		preExisting: preExisting,
	}, nil
}

// Count 返回本次写入的条数。
func (r *Recorder) Count() int { return r.count }

// AppendedToExisting 报告本次是否追加到了已有文件上。
func (r *Recorder) AppendedToExisting() bool { return r.preExisting > 0 }

// Write 写一条事件。
func (r *Recorder) Write(event TapEvent) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("序列化事件失败: %w", err)
	}
	if _, err := r.writer.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("写入事件失败: %w", err)
	}
	if err := r.writer.Flush(); err != nil {
		return fmt.Errorf("刷盘失败: %w", err)
	}
	r.count++
	return nil
}

// Close 关闭文件。
func (r *Recorder) Close() error {
	if err := r.writer.Flush(); err != nil {
		r.file.Close()
		return err
	}
	return r.file.Close()
}

// readEvents 逐行读 jsonl。无法解析的行（截断的尾行等）跳过并返回跳过数。
func readEvents(path string, onEvent func(TapEvent)) (skipped int, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerMaxBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		event, ok := decodeTapEvent(string(line))
		if !ok {
			skipped++
			continue
		}
		onEvent(event)
	}
	return skipped, scanner.Err()
}
