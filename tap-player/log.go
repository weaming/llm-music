package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// 日志输出到 stderr：这个程序在 tmux pane 里跑，pane 本身就是日志，
// 不写文件、不做轮转（仓库里没有轮转先例）。
// 格式对齐 Python 版（JSON 单行），两版日志可以直接对拍。

var (
	logMu     sync.Mutex
	logQuiet  bool
	logOffset = time.FixedZone("CST", 8*3600)
)

// initTimezone 读 TZ，无效则回退 Asia/Hong_Kong。
func initTimezone() {
	name := os.Getenv("TZ")
	if name == "" {
		name = "Asia/Hong_Kong"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Go 在缺 tzdata 的环境里会失败，退回固定偏移并说明
		logOffset = time.FixedZone("CST", 8*3600)
		fmt.Fprintf(os.Stderr, "{\"event\":\"timezone_fallback\",\"want\":%q,\"error\":%q}\n", name, err.Error())
		return
	}
	logOffset = loc
}

// logEvent 记一条 JSON 事件。kv 是交替的键值对：logEvent("x", "k", v, "k2", v2)。
func logEvent(event string, kv ...any) {
	if logQuiet {
		return
	}
	payload := make(map[string]any, len(kv)/2+2)
	payload["ts"] = time.Now().In(logOffset).Format("2006-01-02T15:04:05.000-07:00")
	payload["event"] = event
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		payload[key] = kv[i+1]
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "{\"event\":\"log_marshal_error\",\"error\":%q}\n", err.Error())
		return
	}

	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintln(os.Stderr, string(encoded))
}

// logFatalf 记一条错误并退出。用于启动阶段的不可恢复错误。
func logFatalf(event string, format string, args ...any) {
	logEvent(event, "error", fmt.Sprintf(format, args...))
	os.Exit(1)
}

// shortError 截断过长的错误文本，避免日志被一屏的错误体撑爆。
func shortError(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
