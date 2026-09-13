package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRecorderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tap.jsonl")
	events := []TapEvent{
		{Seq: 1, Ts: "t1", Kind: kindStreamStart, Stream: "s1"},
		{Seq: 2, Ts: "t2", Kind: kindFrame, Stream: "s1", Raw: `data: {"a":1}`},
		{Seq: 3, Ts: "t3", Kind: kindStreamEnd, Stream: "s1"},
	}

	recorder, err := newRecorder(path)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	for _, event := range events {
		if err := recorder.Write(event); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	recorder.Close()

	var restored []TapEvent
	if _, err := readEvents(path, func(event TapEvent) { restored = append(restored, event) }); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(restored) != len(events) {
		t.Fatalf("读回 %d 条，want %d", len(restored), len(events))
	}
	for i := range events {
		// TapEvent 含 map 字段（Usage），不能用 == 比较
		if !reflect.DeepEqual(restored[i], events[i]) {
			t.Errorf("第 %d 条不一致:\n得到 %+v\n期望 %+v", i, restored[i], events[i])
		}
	}
}

func TestRecorderAppends(t *testing.T) {
	// 追加而非覆盖：多次录制累积到同一份素材
	path := filepath.Join(t.TempDir(), "tap.jsonl")
	first, _ := newRecorder(path)
	first.Write(TapEvent{Seq: 1, Ts: "t", Kind: kindFrame})
	first.Close()

	second, _ := newRecorder(path)
	if !second.AppendedToExisting() {
		t.Error("第二次打开应报告追加到了已有文件")
	}
	second.Write(TapEvent{Seq: 2, Ts: "t", Kind: kindFrame})
	second.Close()

	count := 0
	readEvents(path, func(TapEvent) { count++ })
	if count != 2 {
		t.Errorf("读回 %d 条，want 2", count)
	}
}

func TestRecorderCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deep", "tap.jsonl")
	recorder, err := newRecorder(path)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	recorder.Write(TapEvent{Seq: 1, Ts: "t", Kind: kindFrame})
	recorder.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("文件未创建: %v", err)
	}
}

func TestReadSkipsBadLines(t *testing.T) {
	// 录制中途被杀会留下截断的尾行，读取必须容错
	path := filepath.Join(t.TempDir(), "tap.jsonl")
	content := `{"seq":1,"ts":"t","kind":"frame"}
{"seq":2,"ts":"t","kind":"fra

{"seq":3,"ts":"t","kind":"stream_end"}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	var seqs []int64
	skipped, err := readEvents(path, func(event TapEvent) { seqs = append(seqs, event.Seq) })
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 3 {
		t.Errorf("有效行序列 = %v, want [1 3]", seqs)
	}
	if skipped != 1 {
		t.Errorf("跳过 %d 行，want 1", skipped)
	}
}
