package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// 旁路事件的类型。
// tapKindError 表示这次上游请求失败了。没有它,消费端在上游限流时只会看到
// 一片安静,分不清"模型没输出"与"请求被拒了"。
const (
	tapKindStreamStart = "stream_start"
	tapKindFrame       = "frame"
	tapKindStreamEnd   = "stream_end"
	tapKindError       = "error"
)

const (
	tapChanCap    = 256 // 每个订阅者的队列深度
	tapHeartbeat  = 15 * time.Second
	tapTimeLayout = "2006-01-02T15:04:05.000-07:00"
)

// tapEvent 是旁路推送的一条事件。消费端自行决定如何转换,llm-server 不做任何解释。
//
// Raw 保留上游 SSE 帧的原始载荷,逐字节不改(含 data: 前缀与帧内多行)。
// Seq 单调递增且**只在有订阅者时**才分配:旁路队列满时会丢帧,
// 消费端靠 seq 缺口区分「模型没输出」与「推送被丢了」。
// Ts 是 llm-server **读到该帧的时刻**(接收时间),不是上游的时间,
// 也不随消费端所在机器的时钟变化 —— 重放时它是一致的时间轴。
type tapEvent struct {
	Seq    int64     `json:"seq"`
	Ts     string    `json:"ts"`
	Kind   string    `json:"kind"`
	Stream string    `json:"stream,omitempty"`
	Caller string    `json:"caller,omitempty"`
	Event  string    `json:"event,omitempty"`
	Raw    string    `json:"raw,omitempty"`
	Usage  *tapUsage `json:"usage,omitempty"`
	// Status 仅 error 事件带:上游的 HTTP 状态码(429 限流、401 鉴权等),
	// 或本地发起请求失败时的 502。消费端据此决定退避而不是解析 Raw。
	Status int `json:"status,omitempty"`
}

type tapUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

var tapHub = newTapBroadcaster()

// tapSub 是一个旁路订阅者:有界 channel,满时由 publish 丢最旧。
type tapSub struct {
	ch   chan tapEvent
	once sync.Once
}

func (s *tapSub) close() {
	s.once.Do(func() { close(s.ch) })
}

type tapBroadcaster struct {
	mu    sync.RWMutex
	subs  map[*tapSub]struct{}
	seq   atomic.Int64
	count atomic.Int64 // 订阅者数量:让无订阅者时的快路径完全不加锁
}

func newTapBroadcaster() *tapBroadcaster {
	return &tapBroadcaster{subs: make(map[*tapSub]struct{})}
}

func (b *tapBroadcaster) subscribe() *tapSub {
	sub := &tapSub{ch: make(chan tapEvent, tapChanCap)}
	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()
	b.count.Add(1)
	return sub
}

// unsubscribe 幂等。先摘出 map 再关 channel:
// publish 持 RLock 期间不会被摘除,摘除后新 publish 也看不到它,故关闭不会撞上发送。
func (b *tapBroadcaster) unsubscribe(sub *tapSub) {
	b.mu.Lock()
	if _, ok := b.subs[sub]; ok {
		delete(b.subs, sub)
		b.count.Add(-1)
	}
	b.mu.Unlock()
	sub.close()
}

// active 报告当前是否有订阅者。调用方据此跳过事件的构造开销 ——
// publish 虽然无订阅者时也会立刻返回，但事件本身(尤其逐帧的 strings.Join)
// 是在调用处就先算好的，不短路掉就是白烧。
func (b *tapBroadcaster) active() bool { return b.count.Load() != 0 }

// publish 把事件推给所有订阅者并补上 Seq/Ts。
// 无订阅者时直接返回;队列满时丢最旧再塞新的 —— 实时场景要「此刻」,积压比丢失更糟。
// 本函数**绝不阻塞**,主链路不因旁路而受影响。
func (b *tapBroadcaster) publish(ev tapEvent) {
	if b.count.Load() == 0 {
		return
	}

	ev.Seq = b.seq.Add(1)
	ev.Ts = time.Now().Format(tapTimeLayout)

	b.mu.RLock()
	defer b.mu.RUnlock()
	for sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			select {
			case <-sub.ch:
			default:
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}

// tapUsageFrom 把内部用量转成旁路事件里的用量;全零时返回 nil(不代表有信息)。
func tapUsageFrom(u tokenUsage) *tapUsage {
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return nil
	}
	return &tapUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

// handleTap 是旁路订阅端点(GET /v1/tap,SSE)。
// 只推实时事件,不做历史补发 —— 消费场景要的是「此刻」。
func handleTap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, "只支持 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, "当前连接不支持流式响应", http.StatusInternalServerError)
		return
	}

	// 旁路是长连接,必须清掉本次响应的写超时:http.Server.WriteTimeout 是
	// **整条响应**的硬上限、不随 Flush 重置,不清的话连接会在 upstreamTimeout 被掐断。
	// 只清这一个 handler,普通请求仍保留原有保护。
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	// 先订阅再宣告就绪:否则消费端可能一收到响应头就发起请求,
	// 那时订阅还没建立,开头的事件会丢。
	sub := tapHub.subscribe()
	defer tapHub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(tapHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// SSE 注释行:让消费端能及时发现断线,不污染事件流。
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-sub.ch:
			if !open {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
