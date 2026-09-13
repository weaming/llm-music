package main

import (
	"encoding/json"
	"strings"
)

// tap 事件的 kind 取值，与 go/llm-server/tap.go 的常量一一对应。
const (
	kindStreamStart = "stream_start"
	kindFrame       = "frame"
	kindStreamEnd   = "stream_end"
	kindError       = "error"
)

// TapEvent 是 llm-server 推来的一条旁路事件。
//
// Raw 对 frame 是上游 SSE 帧的原始文本，对 error 是上游错误响应体原文。
// Ts 是 llm-server 读到该帧的时刻（接收时间），不是上游时间。
type TapEvent struct {
	Seq    int64          `json:"seq"`
	Ts     string         `json:"ts"`
	Kind   string         `json:"kind"`
	Stream string         `json:"stream,omitempty"`
	Caller string         `json:"caller,omitempty"`
	Event  string         `json:"event,omitempty"`
	Raw    string         `json:"raw,omitempty"`
	Usage  map[string]any `json:"usage,omitempty"`
	Status int            `json:"status,omitempty"`
}

// decodeTapEvent 从一行 JSON 解析事件。无法解析时返回 false
// （录制文件可能有截断的尾行）。
func decodeTapEvent(line string) (TapEvent, bool) {
	var event TapEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return TapEvent{}, false
	}
	return event, event.Kind != ""
}

// Delta 是一帧带来的增量。三个通道互斥或并存。
type Delta struct {
	Text      string
	Reasoning string
	ToolName  string
}

// IsEmpty 报告这个增量是否什么都没带。
func (d Delta) IsEmpty() bool {
	return d.Text == "" && d.Reasoning == "" && d.ToolName == ""
}

// extractData 从原始 SSE 帧文本里取出 data 载荷。
// 按 SSE 规范，同一帧内多个 data: 行用换行连接；忽略 event: 行与注释行。
func extractData(raw string) string {
	var parts []string
	for line := range strings.SplitSeq(raw, "\n") {
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			parts = append(parts, strings.TrimPrefix(after, " "))
		}
	}
	return strings.Join(parts, "\n")
}

// parseDelta 把一帧原始 SSE 文本解析成增量。
// 非内容帧（[DONE]、空 choices、坏 JSON）返回 false。
func parseDelta(raw string) (Delta, bool) {
	data := extractData(raw)
	if data == "" || data == "[DONE]" {
		return Delta{}, false
	}

	var payload struct {
		Choices []struct {
			Delta struct {
				Content   string          `json:"content"`
				Reasoning string          `json:"reasoning_content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return Delta{}, false
	}
	if len(payload.Choices) == 0 {
		return Delta{}, false
	}

	choice := payload.Choices[0].Delta
	return Delta{
		Text:      choice.Content,
		Reasoning: choice.Reasoning,
		ToolName:  firstToolName(choice.ToolCalls),
	}, true
}

// firstToolName 取首个工具调用的函数名。
// 流式下参数是分片到达的，只有首片带 name。
func firstToolName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var calls []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil || len(calls) == 0 {
		return ""
	}
	return calls[0].Function.Name
}
