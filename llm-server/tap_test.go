package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTapPublishWithoutSubscriber 守住快路径:没有订阅者时既不分配 seq 也不阻塞。
func TestTapPublishWithoutSubscriber(t *testing.T) {
	hub := newTapBroadcaster()
	hub.publish(tapEvent{Kind: tapKindFrame, Raw: "x"})

	if got := hub.seq.Load(); got != 0 {
		t.Fatalf("无订阅者时不该分配 seq,得到 %d", got)
	}
}

// TestTapPublishDropsOldest 验证队列满时丢最旧、seq 仍单调递增
// (消费端靠 seq 缺口区分「模型没输出」与「推送被丢了」),且 publish 绝不阻塞主链路。
func TestTapPublishDropsOldest(t *testing.T) {
	const extra = 50

	hub := newTapBroadcaster()
	sub := hub.subscribe()
	defer hub.unsubscribe(sub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range tapChanCap + extra {
			hub.publish(tapEvent{Kind: tapKindFrame, Raw: fmt.Sprintf("f%d", i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish 阻塞了主链路")
	}

	if got := len(sub.ch); got != tapChanCap {
		t.Fatalf("队列长度 = %d, want %d", got, tapChanCap)
	}

	first := <-sub.ch
	if want := fmt.Sprintf("f%d", extra); first.Raw != want {
		t.Fatalf("队首 = %q, want %q(最早的 %d 条应被丢弃)", first.Raw, want, extra)
	}

	prev := first.Seq
	for range tapChanCap - 1 {
		ev := <-sub.ch
		if ev.Seq != prev+1 {
			t.Fatalf("seq 不连续: %d → %d", prev, ev.Seq)
		}
		prev = ev.Seq
	}
}

const rateLimitedBody = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`

// newRateLimitedUpstream 起一个永远返回 429 的上游。
func newRateLimitedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, rateLimitedBody)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// TestTapReportsStreamingError 守住那个静默缺口:流式请求被上游拒绝时消费端必须看得到,
// 否则听到的安静分不清"模型没输出"与"请求被拒了"。
func TestTapReportsStreamingError(t *testing.T) {
	upstream := newRateLimitedUpstream(t)
	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "test-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	tapResp, err := http.Get(srv.URL + "/v1/tap")
	if err != nil {
		t.Fatalf("连接旁路失败: %v", err)
	}
	defer tapResp.Body.Close()

	go func() {
		resp, postErr := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if postErr != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	events := collectTapEvents(tapResp.Body, 5*time.Second, tapKindError)
	if len(events) == 0 {
		t.Fatal("流式请求被拒时旁路没有任何事件——消费端会完全静默")
	}

	var ev tapEvent
	if err := json.Unmarshal([]byte(events[len(events)-1].Data), &ev); err != nil {
		t.Fatalf("解析旁路事件失败: %v", err)
	}
	if ev.Kind != tapKindError {
		t.Fatalf("kind = %q, want %q", ev.Kind, tapKindError)
	}
	if ev.Status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", ev.Status)
	}
	if !strings.Contains(ev.Raw, "rate_limit_error") {
		t.Fatalf("raw 未保留上游错误原文: %q", ev.Raw)
	}
	if ev.Stream == "" {
		t.Error("error 事件缺少 stream 标识")
	}
}

// TestTapDeliversEventsForBlockingCall 非流式请求同样产生旁路事件:
// llm-server 内部把它转发成流式,旁路才拿得到逐 token 的过程。
func TestTapDeliversEventsForBlockingCall(t *testing.T) {
	upstream := newSSEUpstream(t, `{"model":"m","choices":[{"delta":{"content":"hi"}}]}`)

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "test-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	tapResp, err := http.Get(srv.URL + "/v1/tap")
	if err != nil {
		t.Fatalf("连接旁路失败: %v", err)
	}
	defer tapResp.Body.Close()

	go func() {
		resp, postErr := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if postErr != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	events := collectTapEvents(tapResp.Body, 5*time.Second)
	kinds := make(map[string]int, len(events))
	for _, rec := range events {
		kinds[rec.Kind]++
	}
	for _, want := range []string{tapKindStreamStart, tapKindFrame, tapKindStreamEnd} {
		if kinds[want] == 0 {
			t.Fatalf("非流式请求缺少 %q 事件,收到 %v", want, events)
		}
	}
}

// TestHandleTapDeliversEvents 端到端:连上旁路 + 触发一次流式请求,
// 断言收到 stream_start / frame / stream_end,且 frame 原样保留上游载荷。
func TestHandleTapDeliversEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	restoreEnv := setTestLLMEnv("test-key", upstream.URL, "test-model")
	defer restoreEnv()

	srv := httptest.NewServer(router())
	defer srv.Close()

	tapResp, err := http.Get(srv.URL + "/v1/tap")
	if err != nil {
		t.Fatalf("连接旁路失败: %v", err)
	}
	defer tapResp.Body.Close()

	if ct := tapResp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("旁路 Content-Type = %q, want text/event-stream", ct)
	}

	// 收到响应头即说明订阅已建立(handleTap 先订阅再宣告就绪),此时再触发流式请求
	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	events := collectTapEvents(tapResp.Body, 5*time.Second)
	byKind := make(map[string][]tapEvent, len(events))
	for _, rec := range events {
		var ev tapEvent
		if err := json.Unmarshal([]byte(rec.Data), &ev); err != nil {
			t.Fatalf("解析旁路事件失败: %v (data=%q)", err, rec.Data)
		}
		if ev.Seq == 0 || ev.Ts == "" {
			t.Fatalf("%s 事件缺少 seq/ts: %+v", rec.Kind, ev)
		}
		byKind[rec.Kind] = append(byKind[rec.Kind], ev)
	}

	for _, want := range []string{tapKindStreamStart, tapKindFrame, tapKindStreamEnd} {
		if len(byKind[want]) == 0 {
			t.Fatalf("旁路缺少 %q 事件,收到 %v", want, events)
		}
	}

	frame := byKind[tapKindFrame][0]
	if !strings.HasPrefix(frame.Raw, "data: ") || !strings.Contains(frame.Raw, `"content":"hi"`) {
		t.Fatalf("frame.raw = %q, want 原样保留 data: 前缀与上游载荷", frame.Raw)
	}
	if frame.Stream == "" {
		t.Fatal("frame 缺少 stream 标识(并发时需要它区分来源)")
	}
}

type tapRec struct {
	Kind string
	Data string
}

// collectTapEvents 读到任一 stopKinds（默认 stream_end）或超时为止,返回收到的 SSE 事件。
func collectTapEvents(body io.Reader, timeout time.Duration, stopKinds ...string) []tapRec {
	if len(stopKinds) == 0 {
		stopKinds = []string{tapKindStreamEnd}
	}
	lines := make(chan string, 128)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	var (
		events []tapRec
		cur    tapRec
	)
	deadline := time.After(timeout)
	for {
		select {
		case line, open := <-lines:
			if !open {
				return events
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				cur = tapRec{Kind: strings.TrimPrefix(line, "event: ")}
			case strings.HasPrefix(line, "data: "):
				cur.Data = strings.TrimPrefix(line, "data: ")
			case line == "" && cur.Kind != "":
				events = append(events, cur)
				if slices.Contains(stopKinds, cur.Kind) {
					return events
				}
				cur = tapRec{}
			}
		case <-deadline:
			return events
		}
	}
}
