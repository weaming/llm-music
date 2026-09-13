package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// 消费 /v1/tap 的 SSE 客户端。
//
// 重连策略：指数退避 1s→30s，连上即重置。
// 注意 /v1/tap 是 fire-and-forget——无历史补发、无游标，所以这里**不做续传**，
// 只用 seq 缺口判定"推送被丢弃"并记日志：这是区分"模型没输出"与"旁路丢帧"的唯一信号。

const (
	reconnectInitialSec = 1.0
	reconnectMaxSec     = 30.0
	connectTimeoutSec   = 10.0
	scannerMaxBytes     = 4 * 1024 * 1024
)

// TapClient 持续产出 tap 事件，断线自动重连。
type TapClient struct {
	url     string
	client  *http.Client
	stopped atomic.Bool
	lastSeq int64
	gaps    atomic.Int64
}

// newTapClient 构造客户端。
// 刻意**不设** http.Client.Timeout——那会掐断长连接；只给连接与响应头设超时。
// 代理走标准环境变量（tap 是本地回环，正常情况不该被代理）。
func newTapClient(url string) *TapClient {
	return &TapClient{
		url: url,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: connectTimeoutSec * time.Second}).DialContext,
				ResponseHeaderTimeout: connectTimeoutSec * time.Second,
				Proxy:                 http.ProxyFromEnvironment,
			},
		},
	}
}

// Stop 请求停止。当前阻塞的读会在连接关闭后返回。
func (c *TapClient) Stop() { c.stopped.Store(true) }

// Gaps 返回累计检测到的 seq 缺口数。
func (c *TapClient) Gaps() int64 { return c.gaps.Load() }

// Run 持续消费直到 ctx 取消或 Stop 被调用。每个事件调用 emit。
func (c *TapClient) Run(ctx context.Context, emit func(TapEvent)) {
	delay := reconnectInitialSec
	for !c.stopped.Load() {
		if ctx.Err() != nil {
			return
		}
		err := c.consumeOnce(ctx, emit)
		if c.stopped.Load() || ctx.Err() != nil {
			return
		}
		if err != nil {
			logEvent("tap_disconnected", "url", c.url, "retry_in", delay, "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(delay * float64(time.Second))):
		}
		delay = min(delay*2, reconnectMaxSec)
	}
}

// consumeOnce 建立一次连接并读到断开。
func (c *TapClient) consumeOnce(ctx context.Context, emit func(TapEvent)) error {
	logEvent("tap_connecting", "url", c.url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tap 返回 %d", resp.StatusCode)
	}
	logEvent("tap_connected", "url", c.url)
	return c.readStream(resp.Body, emit)
}

// readStream 逐行解析 SSE。空行 = 一帧结束；`:` 开头是心跳注释。
func (c *TapClient) readStream(body io.Reader, emit func(TapEvent)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), scannerMaxBytes)

	var dataLines []string
	var eventName string

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")

		if line == "" {
			if len(dataLines) > 0 {
				if event, ok := decodeTapEvent(strings.Join(dataLines, "\n")); ok {
					if event.Kind == "" && eventName != "" {
						// data 里没有 kind 时回退到 SSE 的 event 名
						event.Kind = eventName
					}
					c.checkGap(event)
					emit(event)
				}
			}
			dataLines, eventName = nil, ""
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue // 心跳注释
		}
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			eventName = strings.TrimSpace(rest)
		} else if rest, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(rest, " "))
		}
	}
	return scanner.Err()
}

// checkGap 检测 seq 跳号。seq 是进程级单调计数器，跳号意味着旁路队列满、丢了事件。
func (c *TapClient) checkGap(event TapEvent) {
	if c.lastSeq > 0 && event.Seq > c.lastSeq+1 {
		missed := event.Seq - c.lastSeq - 1
		total := c.gaps.Add(1)
		logEvent("tap_gap", "missed", missed, "last_seq", c.lastSeq, "seq", event.Seq, "total_gaps", total)
	}
	if event.Seq > c.lastSeq {
		c.lastSeq = event.Seq
	}
}
