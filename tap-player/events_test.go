package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func frameWith(delta map[string]any) string {
	payload := map[string]any{
		"id": "x",
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": nil,
		}},
	}
	encoded, _ := json.Marshal(payload)
	return "data: " + string(encoded)
}

func TestParseTextDelta(t *testing.T) {
	delta, ok := parseDelta(frameWith(map[string]any{"content": "你好"}))
	if !ok {
		t.Fatal("应解析成功")
	}
	if delta.Text != "你好" || delta.Reasoning != "" || delta.ToolName != "" {
		t.Errorf("解析结果异常: %+v", delta)
	}
}

func TestParseReasoningDelta(t *testing.T) {
	delta, ok := parseDelta(frameWith(map[string]any{"reasoning_content": "让我想想"}))
	if !ok {
		t.Fatal("应解析成功")
	}
	if delta.Reasoning != "让我想想" || delta.Text != "" {
		t.Errorf("解析结果异常: %+v", delta)
	}
}

func TestParseToolCallDelta(t *testing.T) {
	raw := frameWith(map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"name": "bash", "arguments": ""},
		}},
	})
	delta, ok := parseDelta(raw)
	if !ok {
		t.Fatal("应解析成功")
	}
	if delta.ToolName != "bash" {
		t.Errorf("ToolName = %q, want bash", delta.ToolName)
	}
}

func TestParseToolCallWithoutName(t *testing.T) {
	// 参数分片到达时没有 name，不该崩
	raw := frameWith(map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"arguments": `{"a"`},
		}},
	})
	delta, ok := parseDelta(raw)
	if !ok {
		t.Fatal("应解析成功")
	}
	if !delta.IsEmpty() {
		t.Errorf("无 name 无内容应视为空，得到 %+v", delta)
	}
}

func TestParseDoneReturnsFalse(t *testing.T) {
	if _, ok := parseDelta("data: [DONE]"); ok {
		t.Error("[DONE] 不应产生增量")
	}
}

func TestParseEmptyChoicesReturnsFalse(t *testing.T) {
	// usage 收尾帧的 choices 是空数组
	if _, ok := parseDelta(`data: {"choices":[],"usage":{"total_tokens":1}}`); ok {
		t.Error("空 choices 不应产生增量")
	}
}

func TestParseBrokenJSONReturnsFalse(t *testing.T) {
	if _, ok := parseDelta("data: {not json"); ok {
		t.Error("坏 JSON 不应产生增量")
	}
}

func TestExtractDataJoinsMultipleLines(t *testing.T) {
	// SSE 规范：同一帧内多个 data: 行用换行连接
	if got := extractData("event: message\ndata: line1\ndata: line2"); got != "line1\nline2" {
		t.Errorf("extractData = %q, want \"line1\\nline2\"", got)
	}
}

func TestExtractDataIgnoresEventAndComments(t *testing.T) {
	if got := extractData(": ping\nevent: frame\ndata: payload"); got != "payload" {
		t.Errorf("extractData = %q, want payload", got)
	}
}

func TestTapEventRoundTrip(t *testing.T) {
	original := TapEvent{
		Seq: 7, Ts: "2026-09-13T17:05:35.693+08:00", Kind: kindFrame,
		Stream: "chatcmpl-1", Caller: "xbot", Raw: "data: {}",
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	restored, ok := decodeTapEvent(string(encoded))
	if !ok {
		t.Fatal("反序列化失败")
	}
	// TapEvent 含 map 字段（Usage），不能用 == 比较
	if !reflect.DeepEqual(restored, original) {
		t.Errorf("往返不一致:\n得到 %+v\n期望 %+v", restored, original)
	}
}

func TestTapEventOmitsEmptyFields(t *testing.T) {
	encoded, _ := json.Marshal(TapEvent{Seq: 1, Ts: "t", Kind: kindStreamEnd})
	var payload map[string]any
	json.Unmarshal(encoded, &payload)
	for _, key := range []string{"raw", "stream", "caller", "event"} {
		if _, exists := payload[key]; exists {
			t.Errorf("空字段 %q 不该出现在 JSON 里", key)
		}
	}
}

func TestDecodeTapEventRejectsBadLine(t *testing.T) {
	if _, ok := decodeTapEvent("not json"); ok {
		t.Error("坏 JSON 应被拒绝")
	}
	if _, ok := decodeTapEvent("[1,2,3]"); ok {
		t.Error("非对象应被拒绝")
	}
	if _, ok := decodeTapEvent(`{"seq":1}`); ok {
		t.Error("没有 kind 应被拒绝")
	}
}
