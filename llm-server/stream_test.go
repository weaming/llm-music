package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStreamingUsageLoggedWithoutTapSubscriber 守住一个容易踩的坑:
// 为了省开销把"帧累加"短路掉之后,finishFrame 若仍以 len(frame)==0 早退,
// 就会把用量提取一并跳过。这里确保**没有旁路订阅者**时用量照样落盘。
func TestStreamingUsageLoggedWithoutTapSubscriber(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "usage.jsonl")
	t.Setenv("LLM_USAGE_LOG_PATH", logPath)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"model\":\"stub-model\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":4,\"total_tokens\":11}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "test-model")
	defer restoreEnv()

	srv := httptest.NewServer(http.HandlerFunc(handleOpenAIChatCompletions))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(
		`{"stream":true,"stream_options":{"include_usage":true},`+
			`"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取用量日志失败: %v（流式路径没落日志）", err)
	}

	var entry usageLogEntry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		t.Fatalf("解析用量日志失败: %v (raw=%s)", err, raw)
	}
	if entry.TokenUsageSource != "provider" {
		t.Fatalf("token_usage_source = %q, want provider（usage 帧没被提取出来）", entry.TokenUsageSource)
	}
	if entry.TotalTokens != 11 || entry.Model != "stub-model" {
		t.Fatalf("用量条目异常: %+v", entry)
	}
}

// TestProbeChatRequest 只摘 model/stream/正文,其余字段一律不碰(由原样转发保真)。
func TestProbeChatRequest(t *testing.T) {
	probe, err := probeChatRequest([]byte(
		`{"model":"m","stream":true,"tools":[],"temperature":0.3,` +
			`"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null}]}`))
	if err != nil {
		t.Fatalf("probe 失败: %v", err)
	}
	if probe.Model != "m" {
		t.Fatalf("Model = %q, want m", probe.Model)
	}
	if !probe.Stream {
		t.Fatal("Stream = false, want true")
	}
	if probe.InputText != "hi" {
		t.Fatalf("InputText = %q, want hi(content 为 null 的消息不计入)", probe.InputText)
	}

	if _, err := probeChatRequest([]byte(`not json`)); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}

// TestHandleStreamingChatRelaysUpstreamError 上游在开流前拒绝时,
// 状态码与错误信封必须原样透传(旧实现无条件压成 502)。
func TestHandleStreamingChatRelaysUpstreamError(t *testing.T) {
	const upstreamBody = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "test-model")
	defer restoreEnv()

	srv := httptest.NewServer(http.HandlerFunc(handleOpenAIChatCompletions))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if string(body) != upstreamBody {
		t.Fatalf("错误响应未被原样回传: %s", body)
	}
	if resp.Header.Get("Retry-After") != "9" {
		t.Fatalf("Retry-After 未转发: %q", resp.Header.Get("Retry-After"))
	}
}

// TestHandleStreamingChatForwardsFrames 验证两点:转发字节与上游逐帧一致,
// 且是**真流式** —— 首帧远早于整条流结束,而不是缓冲完再一次吐出。
func TestHandleStreamingChatForwardsFrames(t *testing.T) {
	const (
		frameCount = 5
		frameGap   = 50 * time.Millisecond
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("上游测试服务不支持 Flush")
			return
		}
		for i := range frameCount {
			fmt.Fprintf(w, "data: {\"index\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(frameGap)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "test-model")
	defer restoreEnv()

	srv := httptest.NewServer(http.HandlerFunc(handleOpenAIChatCompletions))
	defer srv.Close()

	startedAt := time.Now()
	resp, err := http.Post(srv.URL, "application/json",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream(说明没走流式分支)", ct)
	}

	var (
		got      strings.Builder
		firstAt  time.Duration
		hasFirst bool
	)
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			if !hasFirst {
				hasFirst, firstAt = true, time.Since(startedAt)
			}
			got.WriteString(line)
		}
		if readErr != nil {
			break
		}
	}
	total := time.Since(startedAt)

	// 能走到这里本身就证明了一件事:上游发完 [DONE] 之后连接被关掉了。
	// 这是硬要求 —— openai SDK 对 [DONE] 只 continue 不 break(会继续读到 body 结束),
	// 且 xbot 的流式路径没有超时兜底,代理若挂着不关,消费端的 for await 会永久阻塞。

	var want strings.Builder
	for i := range frameCount {
		fmt.Fprintf(&want, "data: {\"index\":%d}\n\n", i)
	}
	want.WriteString("data: [DONE]\n\n")

	if got.String() != want.String() {
		t.Fatalf("转发字节与上游不一致:\n得到 %q\n期望 %q", got.String(), want.String())
	}
	if !hasFirst {
		t.Fatal("一帧都没收到")
	}
	if firstAt > total/2 {
		t.Fatalf("首帧 %v 到达、整条流 %v 才结束 —— 像是被缓冲了,不是真流式", firstAt, total)
	}
}

// TestNormalizeCacheUsage 守住从 callLLM 抽出来的那段用量补齐逻辑。
func TestNormalizeCacheUsage(t *testing.T) {
	partial := tokenUsage{PromptTokens: 100}
	partial.PromptTokenDetail.CachedTokens = 40

	got := normalizeCacheUsage(partial)
	if got.PromptCacheHit != 40 {
		t.Fatalf("PromptCacheHit = %d, want 40", got.PromptCacheHit)
	}
	if got.PromptCacheMiss != 60 {
		t.Fatalf("PromptCacheMiss = %d, want 60", got.PromptCacheMiss)
	}

	full := tokenUsage{PromptTokens: 100, PromptCacheHit: 30, PromptCacheMiss: 70}
	if got := normalizeCacheUsage(full); got != full {
		t.Fatalf("已完整的用量被改动: %+v, want %+v", got, full)
	}
}
