package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// streamFrame 只摘取转发之外还需要用到的少量字段(用量日志)。
// 帧的其余内容一律原样透传,不做解释。
type streamFrame struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage tokenUsage `json:"usage"`
}

// handleStreamingChat 把上游 SSE 逐帧透传给客户端,同时把每一帧原样发布到旁路。
//
// 之所以逐行读而不是 io.Copy:要在**原样转发**的同时识别帧边界(空行)才能发旁路,
// 逐行是唯一能兼顾两者的方式。也不做 SSE 归一化 —— data: 前缀、空行、[DONE]、
// 注释行都按原样透传,保证客户端收到的字节与上游一致。
func handleStreamingChat(w http.ResponseWriter, r *http.Request, relay chatRelay) {
	startedAt := time.Now()
	// 先起 stream 标识:失败路径也要带上它,否则消费端无法把错误归属到某次请求
	streamID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	resp, err := forwardUpstream(r.Context(), relay.Body)
	if err != nil {
		tapHub.publish(tapEvent{
			Kind: tapKindError, Stream: streamID, Caller: relay.Caller,
			Status: http.StatusBadGateway, Raw: err.Error(),
		})
		writeError(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 上游拒绝:原样回传它的状态码与错误信封 —— 客户端靠 429/401 等做退避决策,
		// 压成统一的 502 会让它无法区分"该重试"和"请求本身错了"。
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			tapHub.publish(tapEvent{
				Kind: tapKindError, Stream: streamID, Caller: relay.Caller,
				Status: http.StatusBadGateway, Raw: readErr.Error(),
			})
			writeError(w, fmt.Sprintf("读取上游响应失败: %v", readErr), http.StatusBadGateway)
			return
		}
		tapHub.publish(tapEvent{
			Kind: tapKindError, Stream: streamID, Caller: relay.Caller,
			Status: resp.StatusCode, Raw: string(body),
		})
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
		frame      []string // 当前帧的原始行
		frameData  string   // 当前帧 data: 载荷(多行 data: 以 \n 连接)
		frameEvent string   // 当前帧 event: 名(OpenAI 通常为空)
		content    strings.Builder
		usage      tokenUsage
		modelName  = relay.Model
	)

	// finishFrame 收一个完整帧:原样发往旁路,并摘出 model / 输出正文 / usage。
	//
	// 注意:早退**不能**以 len(frame)==0 为条件 —— 没有旁路订阅者时 frame 根本不会被
	// 累加，那样会把下面的用量提取一并跳过。判据只能是 frameData。
	finishFrame := func() {
		if len(frame) > 0 {
			if tapHub.active() {
				tapHub.publish(tapEvent{
					Kind: tapKindFrame, Stream: streamID, Event: frameEvent,
					Raw: strings.Join(frame, "\n"),
				})
			}
			frame = frame[:0]
		}
		frameEvent = ""

		data := frameData
		frameData = ""
		if data == "" || data == "[DONE]" {
			return
		}

		var parsed streamFrame
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
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
				trimmed := strings.TrimRight(line, "\r\n")
				if trimmed == "" {
					finishFrame()
				} else {
					if tapHub.active() {
						frame = append(frame, trimmed)
					}
					switch {
					case strings.HasPrefix(trimmed, "data:"):
						chunk := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
						if frameData != "" {
							frameData += "\n"
						}
						frameData += chunk
					case strings.HasPrefix(trimmed, "event:"):
						frameEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
					}
				}
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				log.Printf("[LLM] caller=%s 流式读取上游中断: %v", relay.Caller, readErr)
			}
			break
		}
	}
	finishFrame() // 流尾没有空行收尾的残留帧

	text := content.String()
	usage = normalizeCacheUsage(usage)

	tapHub.publish(tapEvent{
		Kind: tapKindStreamEnd, Stream: streamID, Caller: relay.Caller, Usage: tapUsageFrom(usage),
	})

	logTokenUsage(relay.Caller, llmCallResult{
		content:               text,
		model:                 modelName,
		usage:                 usage,
		duration:              time.Since(startedAt),
		outputChars:           len([]rune(text)),
		inputChars:            len([]rune(relay.InputText)),
		estimatedInputTokens:  estimateTokens(relay.InputText),
		estimatedOutputTokens: estimateTokens(text),
		hasTokenUsage:         tapUsageFrom(usage) != nil,
	})

	if !started {
		// 上游一帧都没给,还没宣告 200:回正常的 HTTP 错误,不当成"成功但空"
		writeError(w, "上游未返回任何数据", http.StatusBadGateway)
	}
}
