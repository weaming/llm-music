package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestRequestCaller(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if caller := requestCaller(request); caller != "unknown" {
		t.Fatalf("caller = %q, want unknown", caller)
	}

	request.Header.Set("X-LLM-Caller", "rss cli/1")
	if caller := requestCaller(request); caller != "rsscli1" {
		t.Fatalf("caller = %q, want rsscli1", caller)
	}
}

func TestEstimateTokens(t *testing.T) {
	if tokens := estimateTokens("你好abc"); tokens != 3 {
		t.Fatalf("tokens = %d, want 3", tokens)
	}
}

func TestMessageContent(t *testing.T) {
	if text := messageContent(json.RawMessage(`"hello"`)); text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}

	array := json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if text := messageContent(array); text != "a\nb" {
		t.Fatalf("text = %q, want a\\nb", text)
	}

	if text := messageContent(nil); text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
}

// TestOpenAIRequestForwardedVerbatim 是透传改造的核心断言:
// 客户端发出的 body 必须**逐字节**到达上游 —— tools 的顺序、未知字段、
// 多轮 messages(含 assistant.tool_calls 与 role:"tool" 的 tool_call_id)都不能被改动。
// 这些都是旧实现会静默丢弃或压平的内容。
func TestOpenAIRequestForwardedVerbatim(t *testing.T) {
	// 照抄 xbot 真实发出的形状:xbot 侧 tools 顺序是契约的一部分且无工具时发 []。
	const clientBody = `{"model":"deepseek-v4-flash",` +
		`"messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function",` +
		`"function":{"name":"zeta","arguments":"{}"},"thought_signature":"sig"}]},` +
		`{"role":"tool","tool_call_id":"c1","name":"zeta","content":"ok"}` +
		`],` +
		`"tools":[{"type":"function","function":{"name":"zeta","parameters":{"type":"object","properties":{}}}},` +
		`{"type":"function","function":{"name":"alpha","parameters":{"type":"object","properties":{}}}}],` +
		`"tool_choice":"auto","temperature":0.3,"max_tokens":500,` +
		`"thinking":{"type":"enabled"},"reasoning_effort":"high",` +
		`"unknown_future_field":{"nested":[1,2,3]}}`

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(clientBody))
	recorder := httptest.NewRecorder()
	handleOpenAIChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if string(gotBody) != clientBody {
		t.Fatalf("请求未被原样转发:\n得到 %s\n期望 %s", gotBody, clientBody)
	}
}

// TestOpenAIResponseRelayedVerbatim 上游响应必须原样回传:
// 旧实现从零重建响应,把 finish_reason 硬编码成 "stop"、丢掉 tool_calls 与上游 id。
func TestOpenAIResponseRelayedVerbatim(t *testing.T) {
	const upstreamBody = `{"id":"upstream-id-123","object":"chat.completion","created":1,` +
		`"model":"m","system_fingerprint":"fp_x",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Trace", "trace-1")
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	handleOpenAIChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != upstreamBody {
		t.Fatalf("响应未被原样回传:\n得到 %s\n期望 %s", got, upstreamBody)
	}
	if recorder.Header().Get("X-Upstream-Trace") != "trace-1" {
		t.Fatal("上游响应头未转发")
	}
}

func TestHandleOpenAIChatCompletionsModelFallback(t *testing.T) {
	// model 缺省时服务会补默认值 —— 这是唯一一次改写请求体(重新编码),
	// 其余字段(tools / temperature …)必须原样保留。
	var payload struct {
		Model       string           `json:"model"`
		Tools       []map[string]any `json:"tools"`
		Temperature float64          `json:"temperature"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&payload)
		io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(
		`{"messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"temperature":0.3}`))
	recorder := httptest.NewRecorder()
	handleOpenAIChatCompletions(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if payload.Model != "env-model" {
		t.Fatalf("model = %q, want 回退到环境变量 env-model", payload.Model)
	}
	if len(payload.Tools) != 1 || payload.Temperature != 0.3 {
		t.Fatalf("补默认 model 时丢了其它字段: tools=%v temperature=%v", payload.Tools, payload.Temperature)
	}
}

// TestOpenAIUpstreamErrorRelayed 上游错误必须原样透传:
// 旧实现把所有非 200 压成 500(流式路径还是 502),客户端无法区分"该退避"与"请求本身错了"。
func TestOpenAIUpstreamErrorRelayed(t *testing.T) {
	const upstreamBody = `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	recorder := httptest.NewRecorder()
	handleOpenAIChatCompletions(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429(保留上游状态码)", recorder.Code)
	}
	if recorder.Body.String() != upstreamBody {
		t.Fatalf("错误响应未被原样回传: %s", recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") != "7" {
		t.Fatalf("Retry-After 未转发: %q", recorder.Header().Get("Retry-After"))
	}
}

// TestUpstreamConnectionsReused 守住连接池复用:连续多个请求必须复用同一条 TCP 连接。
// 改造前每个请求都新建一个 Transport,上游会看到 rounds 条连接;共享之后应该只有 1 条。
func TestUpstreamConnectionsReused(t *testing.T) {
	const rounds = 3

	var conns int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	upstream.Start()
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "env-model")
	defer restoreEnv()

	for i := range rounds {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		recorder := httptest.NewRecorder()
		handleOpenAIChatCompletions(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求失败: %d %s", i+1, recorder.Code, recorder.Body.String())
		}
	}

	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("上游接受了 %d 条 TCP 连接, want 1(说明连接池没被复用)", got)
	}
}

// TestUpstreamProxyReadPerCall 守住"OPENAI_PROXY 每次现读"这一既有行为。
// Transport 对每个请求都会调用 Proxy（connectMethodForRequest），所以共享 Transport 后
// 改这个环境变量依然无需重启 —— 不要"优化"成启动时解析一次。
func TestUpstreamProxyReadPerCall(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.deepseek.com/v1/chat/completions", nil)

	t.Setenv("OPENAI_PROXY", "http://127.0.0.1:1")
	got, err := upstreamTransport.Proxy(req)
	if err != nil || got == nil || got.Host != "127.0.0.1:1" {
		t.Fatalf("第一次 proxy=%v err=%v, want 127.0.0.1:1", got, err)
	}

	t.Setenv("OPENAI_PROXY", "http://127.0.0.1:2")
	got, err = upstreamTransport.Proxy(req)
	if err != nil || got == nil || got.Host != "127.0.0.1:2" {
		t.Fatalf("第二次 proxy=%v err=%v, want 127.0.0.1:2(说明只在启动时读了一次)", got, err)
	}
}

// setTestLLMEnv 临时覆盖上游 LLM 配置（这三项每次现读环境变量），返回恢复函数。
func setTestLLMEnv(key, baseURL, model string) func() {
	return swapEnv(map[string]string{
		"OPENAI_API_KEY":  key,
		"OPENAI_BASE_URL": baseURL,
		"OPENAI_MODEL":    model,
	})
}

// swapEnv 批量设置环境变量，返回把原值（含"原先不存在"）放回去的恢复函数。
func swapEnv(values map[string]string) func() {
	previous := make(map[string]*string, len(values))
	for name, value := range values {
		old, existed := os.LookupEnv(name)
		if existed {
			previous[name] = &old
		} else {
			previous[name] = nil
		}
		os.Setenv(name, value)
	}

	return func() {
		for name, old := range previous {
			if old == nil {
				os.Unsetenv(name)
				continue
			}
			os.Setenv(name, *old)
		}
	}
}
