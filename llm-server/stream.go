package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ── 上游 SSE 帧 ─────────────────────────────────────────────────────────

// streamFrame 只摘取转发之外还需要用到的少量字段(用量日志、阻塞路径的合成响应)。
// 帧的其余内容一律原样透传,不做解释。
type streamFrame struct {
	ID                string `json:"id"`
	Created           int64  `json:"created"`
	Model             string `json:"model"`
	SystemFingerprint string `json:"system_fingerprint"`
	Choices           []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role             string           `json:"role"`
			Content          string           `json:"content"`
			ReasoningContent string           `json:"reasoning_content"`
			ToolCalls        []streamToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage tokenUsage `json:"usage"`
}

// streamToolCall 是流式返回的工具调用分片:参数分片到达,只有首片带 id 与 name。
type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// sseFrameData 是一帧拆出来的内容。
type sseFrameData struct {
	// Lines 是帧的原始行(已去行尾);无旁路订阅者时为空,省掉拼接开销
	Lines []string
	// Payload 是 data: 行的载荷,多行以 \n 连接
	Payload string
	// Event 是 event: 行的值(OpenAI 通常为空)
	Event string
}

// sseFramer 按帧切分上游 SSE:空行收帧,flush 收掉没有空行收尾的残留帧。
//
// 抽出来是因为两条路径必须用同一套边界判定:流式透传边读边原样下发,
// 阻塞调用读完再合成非流式响应。注意 keepLines 只决定原始行是否累积,
// Payload 无论如何都要拼 —— 否则无订阅者时用量提取会被一并跳过。
type sseFramer struct {
	keepLines func() bool
	onFrame   func(data sseFrameData)

	lines   []string
	payload []string
	event   string
}

func newSSEFramer(keepLines func() bool, onFrame func(sseFrameData)) *sseFramer {
	return &sseFramer{keepLines: keepLines, onFrame: onFrame}
}

// feed 喂入一行(含行尾)。空行即收帧。
func (f *sseFramer) feed(line string) {
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" {
		f.emit()
		return
	}

	if f.keepLines() {
		f.lines = append(f.lines, trimmed)
	}
	switch {
	case strings.HasPrefix(trimmed, "data:"):
		f.payload = append(f.payload, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
	case strings.HasPrefix(trimmed, "event:"):
		f.event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
	}
}

// flush 收掉残留帧。
func (f *sseFramer) flush() { f.emit() }

func (f *sseFramer) emit() {
	data := sseFrameData{
		Lines:   f.lines,
		Payload: strings.Join(f.payload, "\n"),
		Event:   f.event,
	}
	f.lines, f.payload, f.event = nil, nil, ""

	// 心跳之类的空帧不打扰消费端。注意判据不能只看 Lines ——
	// 没有订阅者时它本来就是空的,那样会把用量一并跳过。
	if len(data.Lines) == 0 && data.Payload == "" && data.Event == "" {
		return
	}
	f.onFrame(data)
}

// publishFrame 把一帧原样发到旁路。无订阅者(keepLines 已让 Lines 为空)时不做事。
func publishFrame(streamID string, data sseFrameData) {
	if len(data.Lines) == 0 {
		return
	}
	tapHub.publish(tapEvent{
		Kind: tapKindFrame, Stream: streamID, Event: data.Event, Raw: strings.Join(data.Lines, "\n"),
	})
}

// newCompletionID 生成本次上游请求的标识。
// 失败路径也要带上它,消费端才能把错误归属到某次请求。
func newCompletionID() string {
	return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
}

// publishUpstreamError 报告一次上游请求失败。
// 没有它,消费端在上游限流时只会看到一片安静,分不清"模型没输出"与"请求被拒了"。
func publishUpstreamError(streamID, caller string, status int, raw string) {
	tapHub.publish(tapEvent{
		Kind: tapKindError, Stream: streamID, Caller: caller, Status: status, Raw: raw,
	})
}

// ── 流式路径:逐帧原样下发 ───────────────────────────────────────────────

// handleStreamingChat 把上游 SSE 逐帧透传给客户端,同时把每一帧原样发布到旁路。
//
// 之所以逐行读而不是 io.Copy:要在**原样转发**的同时识别帧边界(空行)才能发旁路,
// 逐行是唯一能兼顾两者的方式。也不做 SSE 归一化 —— data: 前缀、空行、[DONE]、
// 注释行都按原样透传,保证客户端收到的字节与上游一致。
func handleStreamingChat(w http.ResponseWriter, r *http.Request, relay chatRelay) {
	startedAt := time.Now()
	streamID := newCompletionID()

	resp, err := forwardUpstream(r.Context(), relay.Body)
	if err != nil {
		publishUpstreamError(streamID, relay.Caller, http.StatusBadGateway, err.Error())
		writeError(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 上游拒绝:原样回传它的状态码与错误信封 —— 客户端靠 429/401 等做退避决策,
		// 压成统一的 502 会让它无法区分"该重试"和"请求本身错了"。
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			publishUpstreamError(streamID, relay.Caller, http.StatusBadGateway, readErr.Error())
			writeError(w, fmt.Sprintf("读取上游响应失败: %v", readErr), http.StatusBadGateway)
			return
		}
		publishUpstreamError(streamID, relay.Caller, resp.StatusCode, string(body))
		relayUpstream(w, resp, body)
		return
	}

	flusher, _ := w.(http.Flusher)
	started := false

	// 首字节延迟到确实有内容才下发:上游若在开流前就报错,还能用正常的 HTTP 错误回应,
	// 而不是先宣告 200 再吐一条空回复。
	start := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if flusher != nil {
			flusher.Flush()
		}
	}

	tapHub.publish(tapEvent{Kind: tapKindStreamStart, Stream: streamID, Caller: relay.Caller})

	var (
		content   strings.Builder
		usage     tokenUsage
		modelName = relay.Model
	)

	// 只摘 model / 输出正文 / usage,其余不动。
	onFrame := func(data sseFrameData) {
		publishFrame(streamID, data)

		if data.Payload == "" || data.Payload == "[DONE]" {
			return
		}
		var parsed streamFrame
		if err := json.Unmarshal([]byte(data.Payload), &parsed); err != nil {
			return // 无法解析的帧照样透传过了,这里只是不参与计量
		}
		if parsed.Model != "" {
			modelName = parsed.Model
		}
		for _, choice := range parsed.Choices {
			content.WriteString(choice.Delta.Content)
		}
		if tapUsageFrom(parsed.Usage) != nil {
			usage = parsed.Usage
		}
	}

	framer := newSSEFramer(tapHub.active, onFrame)
	reader := bufio.NewReader(resp.Body)
	clientGone := false
	for !clientGone {
		line, readErr := reader.ReadString('\n')

		if line != "" {
			start()
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				// 客户端断开:上游交给 defer Close 收掉,不再读
				log.Printf("[LLM] caller=%s 流式下发失败,客户端可能已断开: %v", relay.Caller, writeErr)
				clientGone = true
			} else if flusher != nil {
				flusher.Flush()
			}

			if !clientGone {
				framer.feed(line)
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[LLM] caller=%s 流式读取上游中断: %v", relay.Caller, readErr)
			}
			break
		}
	}
	framer.flush() // 流尾没有空行收尾的残留帧

	usage = normalizeCacheUsage(usage)
	tapHub.publish(tapEvent{
		Kind: tapKindStreamEnd, Stream: streamID, Caller: relay.Caller, Usage: tapUsageFrom(usage),
	})

	logTokenUsage(relay.Caller, newCallResult(modelName, content.String(), relay.InputText, usage, time.Since(startedAt)))

	if !started {
		// 上游一帧都没给,还没宣告 200:回正常的 HTTP 错误,不当成"成功但空"
		writeError(w, "上游未返回任何数据", http.StatusBadGateway)
	}
}

// ── 阻塞路径:内部走流式,对调用者透明 ────────────────────────────────────
//
// 非流式请求也转发成流式:上游只有流式才逐 token 吐数据,而旁路(音乐、观测)要看的
// 就是这个过程 —— 客户端要的是非流式响应,不该因此失去旁路。
// 改写只发生在请求体(stream=true + stream_options.include_usage),响应由
// blockCollector 读完流后按上游非流式响应的形状重建。

// handleBlockingChat 处理非流式请求。
func handleBlockingChat(w http.ResponseWriter, r *http.Request, relay chatRelay) {
	startedAt := time.Now()
	streamID := newCompletionID()

	body, err := enableUpstreamStream(relay.Body)
	if err != nil {
		writeError(w, fmt.Sprintf("改写为流式请求失败: %v", err), http.StatusBadRequest)
		return
	}
	relay.Body = body

	resp, err := forwardUpstream(r.Context(), body)
	if err != nil {
		publishUpstreamError(streamID, relay.Caller, http.StatusBadGateway, err.Error())
		writeError(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			publishUpstreamError(streamID, relay.Caller, http.StatusBadGateway, readErr.Error())
			writeError(w, fmt.Sprintf("读取上游响应失败: %v", readErr), http.StatusBadGateway)
			return
		}
		publishUpstreamError(streamID, relay.Caller, resp.StatusCode, string(raw))
		relayUpstream(w, resp, raw)
		return
	}

	tapHub.publish(tapEvent{Kind: tapKindStreamStart, Stream: streamID, Caller: relay.Caller})

	collector := newBlockCollector(relay.Model)
	framer := newSSEFramer(tapHub.active, func(data sseFrameData) {
		publishFrame(streamID, data)
		collector.feed(data.Payload)
	})

	// 上游原始字节留一份:万一它没按 SSE 回(忽略了 stream),就原样透传
	var raw strings.Builder
	reader := bufio.NewReader(resp.Body)
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			raw.WriteString(line)
			framer.feed(line)
		}
		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[LLM] caller=%s 读取上游流中断: %v", relay.Caller, readErr)
			}
			break
		}
	}
	framer.flush()

	usage := normalizeCacheUsage(collector.usage)
	tapHub.publish(tapEvent{
		Kind: tapKindStreamEnd, Stream: streamID, Caller: relay.Caller, Usage: tapUsageFrom(usage),
	})

	if !collector.seen() {
		// 上游没给 data 帧:原样透传它的响应体,别合成一条空回复
		plain := []byte(raw.String())
		log.Printf("[LLM] caller=%s 上游未按 SSE 返回,原样透传 %d 字节", relay.Caller, len(plain))
		relayUpstream(w, resp, plain)
		logTokenUsage(relay.Caller, summarizeUpstream(relay, plain, time.Since(startedAt)))
		return
	}

	payload, err := collector.response(usage)
	if err != nil {
		writeError(w, fmt.Sprintf("合成响应失败: %v", err), http.StatusInternalServerError)
		return
	}

	copyUpstreamHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(payload); err != nil {
		log.Printf("[LLM] caller=%s 下发响应失败: %v", relay.Caller, err)
	}

	logTokenUsage(relay.Caller, collector.result(relay, usage, time.Since(startedAt)))
}

// blockCollector 把流式增量累积成一条非流式响应。
//
// 按 choice 的 index 分别累积:n>1 时上游会把多个 choice 交错在同一个流里,
// 只认第 0 个会让调用者少收到回复。
type blockCollector struct {
	id          string
	model       string
	fingerprint string
	created     int64

	choices map[int]*blockChoice
	order   []int
	usage   tokenUsage
	// sawFrame 标记上游确实回过 SSE data 帧(用于识别"忽略了 stream"的上游)
	sawFrame bool
}

type blockChoice struct {
	role         string
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    []blockToolCall
	finishReason string
}

// blockToolCall 是拼好的工具调用:参数分片到达,必须按 index 累加。
type blockToolCall struct {
	id   string
	kind string
	name string
	args strings.Builder
}

func newBlockCollector(fallbackModel string) *blockCollector {
	return &blockCollector{model: fallbackModel, choices: map[int]*blockChoice{}}
}

// feed 吃一帧的 data 载荷。
func (c *blockCollector) feed(payload string) {
	if payload == "" || payload == "[DONE]" {
		return
	}

	var frame streamFrame
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return // 解析不了的不参与合成(旁路已把原文发出去了)
	}

	c.sawFrame = true
	if frame.ID != "" {
		c.id = frame.ID
	}
	if frame.Model != "" {
		c.model = frame.Model
	}
	if frame.SystemFingerprint != "" {
		c.fingerprint = frame.SystemFingerprint
	}
	if frame.Created != 0 {
		c.created = frame.Created
	}

	for _, choice := range frame.Choices {
		acc := c.choice(choice.Index)
		if choice.Delta.Role != "" {
			acc.role = choice.Delta.Role
		}
		acc.content.WriteString(choice.Delta.Content)
		acc.reasoning.WriteString(choice.Delta.ReasoningContent)
		acc.appendToolCalls(choice.Delta.ToolCalls)
		if choice.FinishReason != "" {
			acc.finishReason = choice.FinishReason
		}
	}

	if tapUsageFrom(frame.Usage) != nil {
		c.usage = frame.Usage
	}
}

func (c *blockCollector) choice(index int) *blockChoice {
	if acc, ok := c.choices[index]; ok {
		return acc
	}
	acc := &blockChoice{}
	c.choices[index] = acc
	c.order = append(c.order, index)
	return acc
}

// seen 报告上游是否回过 data 帧。
func (c *blockCollector) seen() bool { return c.sawFrame }

// text 汇总所有 choice 的输出正文,只用于用量口径。
func (c *blockCollector) text() string {
	var builder strings.Builder
	for _, index := range c.order {
		builder.WriteString(c.choices[index].content.String())
	}
	return builder.String()
}

// result 汇总用量口径。与流式路径共用 newCallResult —— 两条路径的口径必须一致,
// 否则同一个调用流式或非流式会得到不同的用量日志。
func (c *blockCollector) result(relay chatRelay, usage tokenUsage, duration time.Duration) llmCallResult {
	return newCallResult(c.model, c.text(), relay.InputText, usage, duration)
}

// response 按上游非流式响应的形状重建响应体。
func (c *blockCollector) response(usage tokenUsage) ([]byte, error) {
	indexes := slices.Clone(c.order)
	slices.Sort(indexes)

	choices := make([]completionChoice, 0, len(indexes))
	for _, index := range indexes {
		choices = append(choices, c.choices[index].completion(index))
	}

	payload := completion{
		ID:                c.id,
		Object:            "chat.completion",
		Created:           c.created,
		Model:             c.model,
		SystemFingerprint: c.fingerprint,
		Choices:           choices,
	}
	if payload.ID == "" {
		payload.ID = newCompletionID()
	}
	if payload.Created == 0 {
		payload.Created = time.Now().Unix()
	}
	if tapUsageFrom(usage) != nil {
		payload.Usage = &usage
	}

	return json.Marshal(payload)
}

// completion 是非流式响应的信封,字段与上游一致。
type completion struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
	Choices           []completionChoice `json:"choices"`
	Usage             *tokenUsage        `json:"usage,omitempty"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role             string               `json:"role"`
	Content          *string              `json:"content"`
	ReasoningContent string               `json:"reasoning_content,omitempty"`
	ToolCalls        []completionToolCall `json:"tool_calls,omitempty"`
}

type completionToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// completion 把累积到的增量拼成一条 message。
func (c *blockChoice) completion(index int) completionChoice {
	content := c.content.String()
	message := completionMessage{
		Role:             c.role,
		ReasoningContent: c.reasoning.String(),
		ToolCalls:        c.completionToolCalls(),
	}
	// 上游在有工具调用时给的是 content:null(而不是空字符串),
	// 两者对客户端含义不同:null 表示"这条消息只带工具调用"。
	if content != "" || len(message.ToolCalls) == 0 {
		message.Content = &content
	}
	if message.Role == "" {
		message.Role = "assistant"
	}

	finish := c.finishReason
	if finish == "" {
		// 流正常结束但上游没给 finish_reason:按有无工具调用补齐
		finish = "stop"
		if len(message.ToolCalls) > 0 {
			finish = "tool_calls"
		}
	}

	return completionChoice{Index: index, Message: message, FinishReason: finish}
}

// appendToolCalls 按 index 把分片拼进已有的工具调用。
func (c *blockChoice) appendToolCalls(fragments []streamToolCall) {
	for _, fragment := range fragments {
		for len(c.toolCalls) <= fragment.Index {
			c.toolCalls = append(c.toolCalls, blockToolCall{})
		}

		call := &c.toolCalls[fragment.Index]
		if fragment.ID != "" {
			call.id = fragment.ID
		}
		if fragment.Type != "" {
			call.kind = fragment.Type
		}
		if fragment.Function.Name != "" {
			call.name = fragment.Function.Name
		}
		call.args.WriteString(fragment.Function.Arguments)
	}
}

func (c *blockChoice) completionToolCalls() []completionToolCall {
	calls := make([]completionToolCall, 0, len(c.toolCalls))
	for _, call := range c.toolCalls {
		kind := call.kind
		if kind == "" {
			kind = "function"
		}

		payload := completionToolCall{ID: call.id, Type: kind}
		payload.Function.Name = call.name
		payload.Function.Arguments = call.args.String()
		calls = append(calls, payload)
	}
	return calls
}
