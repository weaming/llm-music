package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// 上游配置每次现读环境变量，不在启动时缓存：
// .env 由 dotenv.go 的 init 载入，缓存会固定在载入之前读到空值。
func openAIKey() string   { return strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) }
func openAIBase() string  { return strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")) }
func openAIModel() string { return strings.TrimSpace(os.Getenv("OPENAI_MODEL")) }

// upstreamTimeout 限制单次上游 LLM 请求的总时长。
// http.Server 的 WriteTimeout 必须不低于此值，
// 否则上游尚未返回时连接会被提前断开，客户端收到 EOF。
const upstreamTimeout = 10 * time.Minute

// proxyFromConfig 优先使用 OPENAI_PROXY，非空时作为上游 HTTPS 代理；否则回退标准代理环境变量。
func proxyFromConfig(req *http.Request) (*url.URL, error) {
	if proxyURL := os.Getenv("OPENAI_PROXY"); proxyURL != "" {
		return url.Parse(proxyURL)
	}
	return http.ProxyFromEnvironment(req)
}

// upstreamTransport 是全局共享的上游连接池。
//
// 原先每次请求都新建一个 Transport，代价是：每个请求都要重做一次 TCP + TLS 握手
// （到 api.deepseek.com 是数百毫秒量级），而且废弃的 Transport 只能等 GC 回收，
// 其间它持有的空闲连接不会被及时关闭。共享之后连接才真正复用。
//
// 用 DefaultTransport.Clone() 而不是手写字段：HTTP/2 支持与各类超时都保持 Go 的默认值，
// 只覆盖需要改的两项。也不设 DisableCompression —— 要保留 Go 对 gzip 的透明解压，
// 否则流式路径读到的会是压缩字节，按行切帧会直接失效。
//
// Proxy 仍是每个请求现读环境变量（Transport 对每个请求都会调用它），所以 OPENAI_PROXY
// 改后无需重启。
var upstreamTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxyFromConfig
	// 默认的 MaxIdleConnsPerHost 只有 2，对同时跑多个流式请求的代理太小。
	transport.MaxIdleConnsPerHost = 32
	return transport
}()

// upstreamClient 全局共享。http.Client 本身就是并发安全的，配合共享 Transport 复用连接池。
// Timeout 覆盖整条请求（含读 body），因此流式响应同样受 upstreamTimeout 约束。
var upstreamClient = &http.Client{
	Timeout:   upstreamTimeout,
	Transport: upstreamTransport,
}

type tokenUsage struct {
	PromptTokens      int `json:"prompt_tokens"`
	CompletionTokens  int `json:"completion_tokens"`
	TotalTokens       int `json:"total_tokens"`
	PromptCacheHit    int `json:"prompt_cache_hit_tokens"`
	PromptCacheMiss   int `json:"prompt_cache_miss_tokens"`
	PromptTokenDetail struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type upstreamLLMResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage tokenUsage `json:"usage"`
}

type usageLogEntry struct {
	Timestamp            string `json:"timestamp"`
	Caller               string `json:"caller"`
	SessionID            string `json:"session_id,omitempty"`
	Model                string `json:"model"`
	PromptTokens         int    `json:"prompt_tokens"`
	CacheHitTokens       int    `json:"cache_hit_tokens"`
	CacheMissTokens      int    `json:"cache_miss_tokens"`
	CompletionTokens     int    `json:"completion_tokens"`
	TotalTokens          int    `json:"total_tokens"`
	InputChars           int    `json:"input_chars"`
	OutputChars          int    `json:"output_chars"`
	DurationMilliseconds int64  `json:"duration_ms"`
	TokenUsageSource     string `json:"token_usage_source"`
}

func router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/simple", handleChatCompletions)
	mux.HandleFunc("/v1/chat/completions", handleOpenAIChatCompletions)
	mux.HandleFunc("/v1/tap", handleTap)
	mux.HandleFunc("/health", handleHealth)
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "只支持 POST 请求", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, fmt.Sprintf("读取请求失败: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, fmt.Sprintf("解析 JSON 失败: %v", err), http.StatusBadRequest)
		return
	}

	caller := requestCaller(r)
	userMsg := req.UserPrompt
	if len(userMsg) > 200 {
		userMsg = userMsg[:200] + "..."
	}
	if strings.TrimSpace(req.UserPrompt) == "" {
		writeError(w, "user_prompt 不能为空", http.StatusBadRequest)
		return
	}
	log.Printf("[LLM] caller=%s input_chars=%d user=%s", caller, len([]rune(req.SystemPrompt))+len([]rune(req.UserPrompt)), userMsg)

	result, err := callLLM(openAIModel(), req.SystemPrompt, req.UserPrompt)
	if err != nil {
		writeError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	logTokenUsage(caller, result)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResponse{Content: result.content})
}

func writeError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(chatResponse{Error: msg})
}

type llmCallResult struct {
	content               string
	model                 string
	usage                 tokenUsage
	duration              time.Duration
	outputChars           int
	inputChars            int
	estimatedInputTokens  int
	estimatedOutputTokens int
	hasTokenUsage         bool
}

// ── OpenAI 透传路径 ──────────────────────────────────────────────────────
//
// /v1/chat/completions 是**无损失的透传代理**:客户端发什么上游就收到什么,
// 上游回什么客户端就收到什么。服务自身只做三件事 —— 补默认 model、记用量、发旁路。
// 刻意不解析请求结构:一旦解析就会像旧实现那样静默丢掉 tools / tool_choice /
// reasoning_effort 等字段,并把多轮 messages 压平。

// chatRelay 是一次透传转发所需的全部输入。
type chatRelay struct {
	Caller string
	Model  string
	Body   []byte
	// InputText 是 messages 各条 content 的拼接,只用于用量日志的字符口径。
	InputText string
}

// chatProbe 是转发前从请求体里摘出的少量字段;请求体本身仍原样转发。
type chatProbe struct {
	Model     string
	Stream    bool
	InputText string
}

// handleOpenAIChatCompletions 透传 OpenAI Chat Completions 请求。
// body 中的 model 缺省时回退环境变量 OPENAI_MODEL(此时会重新编码请求体)。
func handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "只支持 POST 请求", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, fmt.Sprintf("读取请求失败: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	probe, err := probeChatRequest(body)
	if err != nil {
		writeError(w, fmt.Sprintf("解析 JSON 失败: %v", err), http.StatusBadRequest)
		return
	}

	model := probe.Model
	if model == "" {
		// 客户端没给 model 时才补默认值 —— 这是唯一一次改写请求体。
		model = openAIModel()
		if body, err = injectModel(body, model); err != nil {
			writeError(w, fmt.Sprintf("注入默认 model 失败: %v", err), http.StatusBadRequest)
			return
		}
	}

	relay := chatRelay{Caller: requestCaller(r), Model: model, Body: body, InputText: probe.InputText}
	log.Printf("[LLM] caller=%s model=%s stream=%t input_chars=%d", relay.Caller, model, probe.Stream, len([]rune(probe.InputText)))

	if probe.Stream {
		handleStreamingChat(w, r, relay)
		return
	}
	handleBlockingChat(w, r, relay)
}

// probeChatRequest 只做最小解析:其余字段(含未知字段)一律不碰,由原样转发保真。
func probeChatRequest(body []byte) (chatProbe, error) {
	var raw struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return chatProbe{}, err
	}

	var text strings.Builder
	for _, msg := range raw.Messages {
		text.WriteString(messageContent(msg.Content))
	}
	return chatProbe{Model: raw.Model, Stream: raw.Stream, InputText: text.String()}, nil
}

// injectModel 在客户端没给 model 时补上默认值。会重新编码请求体,
// 所以只在必要时调用 —— 其余情形一律转发原始字节。
func injectModel(body []byte, model string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload["model"] = model
	return json.Marshal(payload)
}

// forwardUpstream 把请求体原样转发给上游,只替换认证与内容类型。
// 透传路径刻意不做 model 改写(compatibleLLMReqParams 只服务 /v1/chat/simple):
// 改写会让客户端收到与请求不符的 model,也破坏了"原样"。
func forwardUpstream(ctx context.Context, body []byte) (*http.Response, error) {
	if openAIKey() == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		currentOpenAIBaseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+openAIKey())

	return upstreamClient.Do(req)
}

// handleBlockingChat 非流式透传:上游响应原样回传,并记用量。
//
// 刻意**不发旁路事件**:tap 只服务逐 token 的流式场景,
// 非流式请求没有可音乐化的过程,发事件只会让消费端收到无法处理的空壳。
func handleBlockingChat(w http.ResponseWriter, r *http.Request, relay chatRelay) {
	startedAt := time.Now()

	resp, err := forwardUpstream(r.Context(), relay.Body)
	if err != nil {
		writeError(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, fmt.Sprintf("读取上游响应失败: %v", err), http.StatusBadGateway)
		return
	}

	relayUpstream(w, resp, respBody)

	result := summarizeUpstream(relay, respBody, time.Since(startedAt))
	logTokenUsage(relay.Caller, result)
}

// relayUpstream 把上游响应原样回传:保留状态码、复制响应头、写出原始 body。
// 错误响应同样原样回传 —— 客户端依赖上游的状态码(429/401…)与错误信封做退避决策。
func relayUpstream(w http.ResponseWriter, resp *http.Response, body []byte) {
	for k, v := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		log.Printf("[LLM] 回传上游响应失败: %v", err)
	}
}

// isHopByHopHeader 判断逐跳头:它们只对单条连接有效,转发会干扰 Go 自己的连接管理。
func isHopByHopHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// summarizeUpstream 从上游非流式响应里摘出用量日志所需的字段。
func summarizeUpstream(relay chatRelay, respBody []byte, duration time.Duration) llmCallResult {
	var raw struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage tokenUsage `json:"usage"`
	}
	_ = json.Unmarshal(respBody, &raw)

	model := raw.Model
	if model == "" {
		model = relay.Model
	}

	var content strings.Builder
	for _, choice := range raw.Choices {
		content.WriteString(messageContent(choice.Message.Content))
	}
	text := content.String()
	usage := normalizeCacheUsage(raw.Usage)

	return llmCallResult{
		content:               text,
		model:                 model,
		usage:                 usage,
		duration:              duration,
		outputChars:           len([]rune(text)),
		inputChars:            len([]rune(relay.InputText)),
		estimatedInputTokens:  estimateTokens(relay.InputText),
		estimatedOutputTokens: estimateTokens(text),
		hasTokenUsage:         tapUsageFrom(usage) != nil,
	}
}

// messageContent 把消息 content 归一为纯文本，兼容字符串与多段数组两种格式。
func messageContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}

	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// buildSimpleRequest 组装并发起上游请求,只服务 /v1/chat/simple(私有协议的扁平化单轮摘要)。
func buildSimpleRequest(model, systemPrompt, userPrompt string) (*http.Response, error) {
	if openAIKey() == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY not set")
	}

	messages := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userPrompt},
	}

	payload := map[string]any{
		"model":    model,
		"messages": messages,
	}

	compatibleLLMReqParams(payload, model)

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", currentOpenAIBaseURL()+"/chat/completions", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+openAIKey())

	return upstreamClient.Do(req)
}

// normalizeCacheUsage 补齐 prompt cache 命中/未命中字段:上游可能只给
// prompt_tokens_details.cached_tokens,而命中数缺失时未命中数也推不出来。
func normalizeCacheUsage(usage tokenUsage) tokenUsage {
	if usage.PromptCacheHit == 0 {
		usage.PromptCacheHit = usage.PromptTokenDetail.CachedTokens
	}
	if usage.PromptCacheMiss == 0 && usage.PromptTokens >= usage.PromptCacheHit {
		usage.PromptCacheMiss = usage.PromptTokens - usage.PromptCacheHit
	}
	return usage
}

func callLLM(model, systemPrompt, userPrompt string) (llmCallResult, error) {
	startedAt := time.Now()

	resp, err := buildSimpleRequest(model, systemPrompt, userPrompt)
	if err != nil {
		return llmCallResult{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return llmCallResult{}, err
	}

	if resp.StatusCode != 200 {
		return llmCallResult{}, fmt.Errorf("LLM API error: %s", string(respBody))
	}

	var result upstreamLLMResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return llmCallResult{}, err
	}

	if len(result.Choices) == 0 {
		return llmCallResult{}, fmt.Errorf("empty LLM response")
	}

	content := result.Choices[0].Message.Content
	if content == "" {
		return llmCallResult{}, fmt.Errorf("empty LLM content")
	}

	usage := normalizeCacheUsage(result.Usage)

	return llmCallResult{
		content:               content,
		model:                 result.Model,
		usage:                 usage,
		duration:              time.Since(startedAt),
		outputChars:           len([]rune(content)),
		inputChars:            len([]rune(systemPrompt)) + len([]rune(userPrompt)),
		estimatedInputTokens:  estimateTokens(systemPrompt + userPrompt),
		estimatedOutputTokens: estimateTokens(content),
		hasTokenUsage:         usage.TotalTokens > 0 || usage.PromptTokens > 0 || usage.CompletionTokens > 0,
	}, nil
}

func requestCaller(r *http.Request) string {
	caller := strings.TrimSpace(r.Header.Get("X-LLM-Caller"))
	if caller == "" {
		return "unknown"
	}

	return strings.Map(func(char rune) rune {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			return char
		}
		return -1
	}, caller)
}

func logTokenUsage(caller string, result llmCallResult) {
	if !result.hasTokenUsage {
		log.Printf("[LLM] caller=%s model=%s input_chars=%d output_chars=%d estimated_input_tokens=%d estimated_output_tokens=%d duration_ms=%d token_usage_source=character_estimate", caller, result.model, result.inputChars, result.outputChars, result.estimatedInputTokens, result.estimatedOutputTokens, result.duration.Milliseconds())
		appendUsageLog(usageLogEntry{
			Timestamp:            time.Now().UTC().Format(time.RFC3339Nano),
			Caller:               caller,
			Model:                result.model,
			PromptTokens:         result.estimatedInputTokens,
			CompletionTokens:     result.estimatedOutputTokens,
			TotalTokens:          result.estimatedInputTokens + result.estimatedOutputTokens,
			InputChars:           result.inputChars,
			OutputChars:          result.outputChars,
			DurationMilliseconds: result.duration.Milliseconds(),
			TokenUsageSource:     "character_estimate",
		})
		return
	}

	log.Printf("[LLM] caller=%s model=%s prompt_tokens=%d cache_hit_tokens=%d cache_miss_tokens=%d completion_tokens=%d total_tokens=%d output_chars=%d duration_ms=%d token_usage_source=provider", caller, result.model, result.usage.PromptTokens, result.usage.PromptCacheHit, result.usage.PromptCacheMiss, result.usage.CompletionTokens, result.usage.TotalTokens, result.outputChars, result.duration.Milliseconds())
	appendUsageLog(usageLogEntry{
		Timestamp:            time.Now().UTC().Format(time.RFC3339Nano),
		Caller:               caller,
		Model:                result.model,
		PromptTokens:         result.usage.PromptTokens,
		CacheHitTokens:       result.usage.PromptCacheHit,
		CacheMissTokens:      result.usage.PromptCacheMiss,
		CompletionTokens:     result.usage.CompletionTokens,
		TotalTokens:          result.usage.TotalTokens,
		InputChars:           result.inputChars,
		OutputChars:          result.outputChars,
		DurationMilliseconds: result.duration.Milliseconds(),
		TokenUsageSource:     "provider",
	})
}

func appendUsageLog(entry usageLogEntry) {
	logPath := os.Getenv("LLM_USAGE_LOG_PATH")
	if logPath == "" {
		logPath = "/app/logs/llm-usage.jsonl"
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		log.Printf("[LLM] 创建 usage 日志目录失败: %v", err)
		return
	}

	encoded, err := json.Marshal(entry)
	if err != nil {
		log.Printf("[LLM] 序列化 usage 日志失败: %v", err)
		return
	}

	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[LLM] 打开 usage 日志失败: %v", err)
		return
	}
	defer file.Close()

	if _, err := file.Write(append(encoded, '\n')); err != nil {
		log.Printf("[LLM] 写入 usage 日志失败: %v", err)
	}
}

func estimateTokens(text string) int {
	if text == "" {
		return 0
	}

	var estimated float64
	for _, char := range text {
		switch {
		case unicode.IsSpace(char):
			estimated += 0.1
		case unicode.Is(unicode.Han, char):
			estimated += 0.6
		default:
			estimated += 0.3
		}
	}

	return int(math.Ceil(estimated))
}

func currentOpenAIBaseURL() string {
	apiBase := openAIBase()
	if apiBase == "" {
		apiBase = "https://api.openai.com/v1"
	}

	if _, err := os.Stat("/.dockerenv"); err != nil && strings.Contains(apiBase, "host.docker.internal") {
		apiBase = strings.ReplaceAll(apiBase, "host.docker.internal", "localhost")
	}

	return apiBase
}

func currentOpenAIModel() string {
	model := openAIModel()
	payload := map[string]any{"model": model}
	compatibleLLMReqParams(payload, model)

	normalized, ok := payload["model"].(string)
	if !ok {
		return model
	}

	return normalized
}

func compatibleLLMReqParams(data map[string]any, model string) {
	if strings.HasPrefix(strings.ToLower(model), "minimax") {
		data["reasoning_split"] = true
		if model, ok := data["model"].(string); ok {
			if !strings.HasSuffix(strings.ToLower(model), "highspeed") {
				data["model"] = model + "-highspeed"
			}
		}
	}
}
