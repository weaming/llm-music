package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// newSSEUpstream 起一个假上游:把 payload 逐个作为 SSE data 帧发出,最后补 [DONE]。
func newSSEUpstream(t *testing.T, payloads ...string) *httptest.Server {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("测试上游不支持 Flush")
			return
		}
		for _, payload := range payloads {
			io.WriteString(w, "data: "+payload+"\n\n")
			flusher.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// postBlocking 发一个非流式请求并把响应读完。
func postBlocking(t *testing.T, srv *httptest.Server, body string) (*http.Response, string) {
	t.Helper()

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return resp, string(raw)
}

// TestBlockingRequestRewrittenToStream 阻塞路径只改两处:stream 置真、要求带 usage。
// 其余字段(tools / temperature / 未知字段)必须原样保留 —— 改写不能变成"重建"。
func TestBlockingRequestRewrittenToStream(t *testing.T) {
	received := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		received <- payload
		io.WriteString(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	const clientBody = `{"model":"m","temperature":0.3,"unknown_future_field":{"nested":[1,2,3]},` +
		`"tools":[{"type":"function","function":{"name":"f"}}],` +
		`"messages":[{"role":"user","content":"hi"}]}`

	if _, body := postBlocking(t, srv, clientBody); body == "" {
		t.Fatal("响应为空")
	}

	got := <-received
	options, ok := got["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage=true", got["stream_options"])
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true", got["stream"])
	}

	var want map[string]any
	if err := json.Unmarshal([]byte(clientBody), &want); err != nil {
		t.Fatalf("解析测试请求失败: %v", err)
	}
	delete(got, "stream")
	delete(got, "stream_options")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("除 stream 外还有字段被改动:\n得到 %v\n期望 %v", got, want)
	}
}

// TestBlockingResponseSynthesizedFromStream 客户端要非流式响应,
// 但 llm-server 内部是流式的:合成出来的形状必须与上游的非流式响应一致,
// 否则调用者能从字段缺失上看出中间隔了一层。
func TestBlockingResponseSynthesizedFromStream(t *testing.T) {
	upstream := newSSEUpstream(t,
		`{"id":"chatcmpl-abc","object":"chat.completion.chunk","created":1700000000,"model":"stub-model",`+
			`"system_fingerprint":"fp_1","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"，世界"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14,`+
			`"prompt_tokens_details":{"cached_tokens":4}}}`,
	)

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	resp, body := postBlocking(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json(非流式调用者不该收到 SSE)", ct)
	}
	if strings.Contains(body, "data: ") {
		t.Fatalf("响应体里漏出了 SSE 帧: %s", body)
	}

	var got completion
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析合成响应失败: %v (body=%s)", err, body)
	}

	if got.ID != "chatcmpl-abc" || got.Model != "stub-model" || got.Created != 1700000000 {
		t.Fatalf("信封字段没沿用上游: %+v", got)
	}
	if got.Object != "chat.completion" || got.SystemFingerprint != "fp_1" {
		t.Fatalf("object/system_fingerprint 异常: %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d 条, want 1", len(got.Choices))
	}

	choice := got.Choices[0]
	if choice.Message.Content == nil || *choice.Message.Content != "你好，世界" {
		t.Fatalf("content = %v, want 你好，世界", choice.Message.Content)
	}
	if choice.Message.Role != "assistant" || choice.FinishReason != "stop" {
		t.Fatalf("role/finish_reason 异常: %+v", choice)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 14 || got.Usage.PromptCacheHit != 4 || got.Usage.PromptCacheMiss != 5 {
		t.Fatalf("usage 未沿用上游或未补齐缓存字段: %+v", got.Usage)
	}
}

// TestBlockingMergesToolCallFragments 流式下的工具调用参数是分片到达的,
// 合成响应必须把分片拼回完整参数,并在有工具调用时给 content:null(而不是空字符串)。
func TestBlockingMergesToolCallFragments(t *testing.T) {
	upstream := newSSEUpstream(t,
		`{"id":"x","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":`+
			`[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},`+
			`"finish_reason":"tool_calls"}]}`,
	)

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	_, body := postBlocking(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(body, `"content":null`) {
		t.Fatalf("有工具调用时 content 应为 null: %s", body)
	}

	var got completion
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析合成响应失败: %v (body=%s)", err, body)
	}

	message := got.Choices[0].Message
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d 条, want 1", len(message.ToolCalls))
	}
	call := message.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "bash" {
		t.Fatalf("工具调用头字段异常: %+v", call)
	}
	if call.Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("参数分片没拼起来: %q", call.Function.Arguments)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
}

// TestBlockingKeepsReasoningContent 推理内容也要带回给调用者:
// 有些客户端靠它展示思考过程,合成响应里丢掉就等于降级。
func TestBlockingKeepsReasoningContent(t *testing.T) {
	upstream := newSSEUpstream(t,
		`{"model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"想"}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"一下"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"答案"}}]}`,
	)

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	_, body := postBlocking(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	var got completion
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析合成响应失败: %v (body=%s)", err, body)
	}

	message := got.Choices[0].Message
	if message.ReasoningContent != "想一下" {
		t.Fatalf("reasoning_content = %q, want 想一下", message.ReasoningContent)
	}
	if message.Content == nil || *message.Content != "答案" {
		t.Fatalf("content = %v, want 答案", message.Content)
	}
}

// TestBlockingSynthesizesMultipleChoices n>1 时上游把多个 choice 交错在同一流里,
// 只累积第 0 个会让调用者少收到回复。输出按 index 排序。
func TestBlockingSynthesizesMultipleChoices(t *testing.T) {
	upstream := newSSEUpstream(t,
		`{"model":"m","choices":[{"index":1,"delta":{"content":"乙"}},{"index":0,"delta":{"content":"甲"}}]}`,
		`{"choices":[{"index":1,"delta":{"content":"。"},"finish_reason":"stop"},`+
			`{"index":0,"delta":{"content":"。"},"finish_reason":"stop"}]}`,
	)

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	_, body := postBlocking(t, srv, `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}]}`)

	var got completion
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("解析合成响应失败: %v (body=%s)", err, body)
	}
	if len(got.Choices) != 2 {
		t.Fatalf("choices = %d 条, want 2", len(got.Choices))
	}

	want := []struct {
		index   int
		content string
	}{{0, "甲。"}, {1, "乙。"}}
	for i, item := range want {
		choice := got.Choices[i]
		if choice.Index != item.index {
			t.Fatalf("第 %d 条 index = %d, want %d(必须按 index 排序)", i, choice.Index, item.index)
		}
		if choice.Message.Content == nil || *choice.Message.Content != item.content {
			t.Fatalf("index %d content = %v, want %s", item.index, choice.Message.Content, item.content)
		}
	}
}

// TestBlockingPublishesFrameForTap 阻塞路径的旁路事件里,frame 必须是上游原文,
// 否则消费端(音乐)拿到的载荷和流式路径不是一回事。
func TestBlockingPublishesFrameForTap(t *testing.T) {
	upstream := newSSEUpstream(t, `{"choices":[{"delta":{"content":"hi"}}]}`)

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	sub := tapHub.subscribe()
	defer tapHub.unsubscribe(sub)

	srv := httptest.NewServer(router())
	defer srv.Close()

	postBlocking(t, srv, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	var (
		kinds     []string
		frameRaws []string
	)
	for len(sub.ch) > 0 {
		event := <-sub.ch
		kinds = append(kinds, event.Kind)
		if event.Kind == tapKindFrame {
			frameRaws = append(frameRaws, event.Raw)
		}
		if event.Stream == "" {
			t.Fatalf("%s 事件缺少 stream 标识", event.Kind)
		}
	}

	// [DONE] 帧也照发(与流式路径一致),所以只看顺序与内容帧的原文
	if kinds[0] != tapKindStreamStart || kinds[len(kinds)-1] != tapKindStreamEnd {
		t.Fatalf("旁路事件序列 = %v, want 以 stream_start 开头、stream_end 结尾", kinds)
	}
	if !slices.Contains(frameRaws, `data: {"choices":[{"delta":{"content":"hi"}}]}`) {
		t.Fatalf("旁路没有原样转发内容帧: %v", frameRaws)
	}
}
