// Package executor provides runtime execution capabilities for various AI service providers.
// This file implements the Kiro (AWS CodeWhisperer) executor — translating OpenAI Chat
// Completions into AWS CodeWhisperer GenerateAssistantResponse requests, decoding
// the AWS EventStream binary streaming response back into OpenAI/SSE chunks, and
// refreshing both AWS SSO OIDC (Builder ID / IDC) and Kiro Social Auth tokens.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	kiroProvider             = "kiro"
	kiroDefaultRegion        = "us-east-1"
	kiroSocialTokenURL       = "https://prod.us-east-1.auth.desktop.kiro.dev/refreshToken"
	kiroAWSAuthHostFmt       = "https://oidc.%s.amazonaws.com/token"
	kiroGenerateResponsePath = "/generateAssistantResponse"
	kiroAPIBaseURLFmt        = "https://codewhisperer.%s.amazonaws.com"
	kiroAuthMethodIDC        = "idc"
	kiroAccessTokenSkew      = 5 * time.Minute
)

// KiroExecutor is the stateless runtime executor for Kiro / AWS CodeWhisperer.
type KiroExecutor struct {
	cfg *config.Config
}

// NewKiroExecutor constructs a Kiro executor bound to the running config.
func NewKiroExecutor(cfg *config.Config) *KiroExecutor { return &KiroExecutor{cfg: cfg} }

// Identifier returns the provider key used by the runtime auth manager.
func (e *KiroExecutor) Identifier() string { return kiroProvider }

// PrepareRequest is intentionally a no-op: the Kiro request body is generated
// per-call inside Execute / ExecuteStream and cannot be authored by callers.
func (e *KiroExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if token := kiroAccessTokenFromAuth(auth); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(req, auth.Attributes)
	}
	return nil
}

// HttpRequest is provided for parity with other executors; Kiro does not use a
// pass-through path so this just attaches the bearer token and sends.
func (e *KiroExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kiro executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming request to Kiro by running a streaming call
// internally and aggregating the resulting deltas into a single OpenAI Chat
// Completions response.
func (e *KiroExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	streamResult, err := e.executeKiro(ctx, auth, req, opts, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer streamResult.close()

	aggregator := newKiroNonStreamAggregator(req.Model)
	for chunk := range streamResult.openAIChunks {
		if chunk.err != nil {
			return cliproxyexecutor.Response{}, chunk.err
		}
		aggregator.feed(chunk.payload)
	}
	openaiBody := aggregator.build()
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, req.Payload, openaiBody, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: streamResult.headers.Clone()}, nil
}

// ExecuteStream performs a streaming request to Kiro and translates the AWS
// EventStream binary frames into OpenAI Chat Completions SSE chunks, then
// re-translates those into the inbound source format on the fly.
func (e *KiroExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	streamResult, err := e.executeKiro(ctx, auth, req, opts, true)
	if err != nil {
		return nil, err
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	out := make(chan cliproxyexecutor.StreamChunk)

	go func() {
		defer close(out)
		defer streamResult.close()
		var param any
		for chunk := range streamResult.openAIChunks {
			if chunk.err != nil {
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: chunk.err}:
				case <-ctx.Done():
					return
				}
				continue
			}
			translated := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, req.Payload, bytes.Clone(chunk.payload), &param)
			for i := range translated {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: translated[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		// Flush translator with the [DONE] sentinel so the SSE stream closes cleanly.
		tail := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, req.Payload, []byte("[DONE]"), &param)
		for i := range tail {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: tail[i]}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: streamResult.headers.Clone(), Chunks: out}, nil
}

// CountTokens is not supported by Kiro upstream; return 501 so callers can
// fall back to local heuristics.
func (e *KiroExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "kiro executor: countTokens not supported"}
}

// Refresh exchanges the stored refresh token for a new access token. Kiro
// supports two flows: AWS SSO OIDC (Builder ID / IDC), and Kiro Social Auth.
func (e *KiroExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("kiro executor: auth is nil")
	}
	updated := auth.Clone()
	if err := e.refreshTokenInPlace(ctx, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

// kiroStreamResult is the internal handle for the upstream HTTP response and
// the channel of OpenAI-shaped SSE chunks produced from the AWS EventStream.
type kiroStreamResult struct {
	headers      http.Header
	openAIChunks <-chan kiroOpenAIChunk
	closeFn      func()
}

func (r *kiroStreamResult) close() {
	if r != nil && r.closeFn != nil {
		r.closeFn()
	}
}

type kiroOpenAIChunk struct {
	payload []byte
	err     error
}

// executeKiro builds the upstream request, sends it, and starts a goroutine
// that converts AWS EventStream frames into OpenAI Chat Completions SSE
// chunks. Both Execute and ExecuteStream funnel through this helper.
func (e *KiroExecutor) executeKiro(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, _ bool) (*kiroStreamResult, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "kiro executor: missing auth"}
	}
	// Always translate the inbound payload to OpenAI shape first; the Kiro
	// request builder consumes the OpenAI Chat Completions schema.
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := req.Payload
	if len(opts.OriginalRequest) > 0 {
		body = opts.OriginalRequest
	}
	openaiBody := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(body), true)

	profileArn := kiroExtractProfileArn(auth.Attributes, auth.Metadata)
	payload := buildKiroRequestPayload(req.Model, openaiBody, profileArn)

	token, err := e.ensureAccessToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	region := kiroRegionForAuth(auth)
	url := fmt.Sprintf(kiroAPIBaseURLFmt, region) + kiroGenerateResponsePath

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload.Body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	for k, v := range buildKiroFingerprintHeaders(auth, token) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	httpReq.Header.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	if auth != nil {
		util.ApplyCustomHeadersFromAttrs(httpReq, auth.Attributes)
	}

	helps.RecordAPIRequest(ctx, e.cfg, kiroUpstreamLog(httpReq, payload.Body, auth, e.Identifier(), url))

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		helps.AppendAPIResponseChunk(ctx, e.cfg, errBody)
		return nil, statusErr{code: httpResp.StatusCode, msg: string(errBody)}
	}

	// Consume the response body asynchronously, decoding EventStream frames
	// and emitting OpenAI Chat Completions SSE chunks.
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), req.Model, auth)
	openAIChunks := make(chan kiroOpenAIChunk, 32)
	bodyCloser := httpResp.Body
	go kiroPumpEventStream(ctx, e.cfg, bodyCloser, payload.UpstreamModel, reporter, openAIChunks)

	return &kiroStreamResult{
		headers:      httpResp.Header.Clone(),
		openAIChunks: openAIChunks,
		closeFn: func() {
			if errClose := bodyCloser.Close(); errClose != nil {
				log.Errorf("kiro executor: close response body error: %v", errClose)
			}
		},
	}, nil
}

// kiroPumpEventStream reads the AWS EventStream framed body, decodes events,
// and emits OpenAI Chat Completions SSE chunks (`data: {...}`) on the channel.
// On scanner / parse errors the channel is closed with an error chunk.
func kiroPumpEventStream(ctx context.Context, cfg *config.Config, body io.ReadCloser, model string, reporter *helps.UsageReporter, out chan<- kiroOpenAIChunk) {
	defer close(out)

	state := newKiroStreamState(ctx, model, reporter)
	dec := &kiroEventStreamDecoder{}
	reader := bufio.NewReaderSize(body, 64*1024)
	buf := make([]byte, 32*1024)

	for {
		n, errRead := reader.Read(buf)
		if n > 0 {
			helps.AppendAPIResponseChunk(ctx, cfg, buf[:n])
			dec.Append(buf[:n])
			for {
				frame, errFrame := dec.Next()
				if errFrame != nil {
					log.Warnf("kiro executor: event stream parse warning: %v", errFrame)
					continue
				}
				if frame == nil {
					break
				}
				for _, chunk := range state.handleFrame(frame) {
					select {
					case out <- kiroOpenAIChunk{payload: chunk}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
		if errRead == io.EOF {
			break
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, cfg, errRead)
			select {
			case out <- kiroOpenAIChunk{err: errRead}:
			case <-ctx.Done():
			}
			return
		}
	}

	for _, chunk := range state.flush() {
		select {
		case out <- kiroOpenAIChunk{payload: chunk}:
		case <-ctx.Done():
			return
		}
	}
}

// kiroStreamState tracks per-stream metadata used to assemble OpenAI deltas
// from individual AWS CodeWhisperer events.
type kiroStreamState struct {
	ctx           context.Context
	model         string
	responseID    string
	createdSec    int64
	chunkIndex    int
	hasToolCalls  bool
	finishEmitted bool

	reporter       *helps.UsageReporter
	usagePublished bool
	hasMetering    bool
	hasContext     bool
	contextPct     float64
	usagePrompt    int64
	usageOutput    int64
	contentLen     int64
	reasoningSeen  int

	toolCallIndex int
	seenToolIDs   map[string]int
}

func newKiroStreamState(ctx context.Context, model string, reporter *helps.UsageReporter) *kiroStreamState {
	return &kiroStreamState{
		ctx:         ctx,
		model:       model,
		responseID:  fmt.Sprintf("chatcmpl-%d", time.Now().UnixMilli()),
		createdSec:  time.Now().Unix(),
		reporter:    reporter,
		seenToolIDs: map[string]int{},
	}
}

// handleFrame interprets one AWS EventStream frame and returns zero or more
// OpenAI Chat Completions SSE chunks (each a fully formed `data: {...}\n\n`).
func (s *kiroStreamState) handleFrame(frame *kiroEventFrame) [][]byte {
	eventType := frame.headers[":event-type"]
	if eventType == "" {
		return nil
	}
	out := [][]byte{}

	switch eventType {
	case "assistantResponseEvent":
		text := gjson.GetBytes(frame.payload, "content").String()
		if text != "" {
			s.contentLen += int64(len(text))
			delta := s.assistantTextDelta(text)
			out = append(out, s.encodeDelta(delta))
		}

	case "reasoningContentEvent":
		text := gjson.GetBytes(frame.payload, "reasoningContentEvent.text").String()
		if text == "" {
			text = gjson.GetBytes(frame.payload, "text").String()
		}
		if text == "" {
			text = gjson.GetBytes(frame.payload, "content").String()
		}
		if text != "" {
			s.contentLen += int64(len(text))
			delta := s.reasoningDelta(text)
			out = append(out, s.encodeDelta(delta))
			s.reasoningSeen++
		}

	case "codeEvent":
		text := gjson.GetBytes(frame.payload, "content").String()
		if text != "" {
			delta := s.assistantTextDelta(text)
			out = append(out, s.encodeDelta(delta))
		}

	case "toolUseEvent":
		s.hasToolCalls = true
		// Payload may be a single object or array of tool uses.
		if gjson.GetBytes(frame.payload, "0").Exists() {
			gjson.GetBytes(frame.payload, "@this").ForEach(func(_, v gjson.Result) bool {
				out = append(out, s.handleToolUse(v)...)
				return true
			})
		} else {
			out = append(out, s.handleToolUse(gjson.ParseBytes(frame.payload))...)
		}

	case "messageStopEvent":
		if !s.finishEmitted {
			out = append(out, s.encodeFinish())
		}

	case "contextUsageEvent":
		s.hasContext = true
		s.contextPct = gjson.GetBytes(frame.payload, "contextUsagePercentage").Float()

	case "meteringEvent":
		s.hasMetering = true

	case "metricsEvent":
		s.usagePrompt = gjson.GetBytes(frame.payload, "metricsEvent.inputTokens").Int()
		s.usageOutput = gjson.GetBytes(frame.payload, "metricsEvent.outputTokens").Int()
		if s.usagePrompt == 0 {
			s.usagePrompt = gjson.GetBytes(frame.payload, "inputTokens").Int()
		}
		if s.usageOutput == 0 {
			s.usageOutput = gjson.GetBytes(frame.payload, "outputTokens").Int()
		}
		s.publishExplicitUsage()
	}

	if s.hasMetering && s.hasContext && !s.finishEmitted {
		out = append(out, s.encodeFinish())
	}
	return out
}

func (s *kiroStreamState) handleToolUse(r gjson.Result) [][]byte {
	out := [][]byte{}
	id := r.Get("toolUseId").String()
	if id == "" {
		id = fmt.Sprintf("call_%d", time.Now().UnixNano())
	}
	name := r.Get("name").String()
	idx, seen := s.seenToolIDs[id]
	if !seen {
		idx = s.toolCallIndex
		s.toolCallIndex++
		s.seenToolIDs[id] = idx
		startDelta := map[string]any{
			"tool_calls": []any{
				map[string]any{
					"index": idx,
					"id":    id,
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": "",
					},
				},
			},
		}
		if s.chunkIndex == 0 {
			startDelta["role"] = "assistant"
		}
		out = append(out, s.encodeDelta(startDelta))
	}

	input := r.Get("input")
	if input.Exists() {
		argsStr := input.Raw
		if input.Type == gjson.String {
			argsStr = input.String()
		}
		if argsStr != "" {
			argsDelta := map[string]any{
				"tool_calls": []any{
					map[string]any{
						"index": idx,
						"function": map[string]any{
							"arguments": argsStr,
						},
					},
				},
			}
			out = append(out, s.encodeDelta(argsDelta))
		}
	}
	return out
}

func (s *kiroStreamState) assistantTextDelta(text string) map[string]any {
	delta := map[string]any{"content": text}
	if s.chunkIndex == 0 {
		delta["role"] = "assistant"
	}
	return delta
}

func (s *kiroStreamState) reasoningDelta(text string) map[string]any {
	delta := map[string]any{"reasoning_content": text}
	if s.chunkIndex == 0 && s.reasoningSeen == 0 {
		delta["role"] = "assistant"
	}
	return delta
}

func (s *kiroStreamState) encodeDelta(delta map[string]any) []byte {
	chunk := map[string]any{
		"id":      s.responseID,
		"object":  "chat.completion.chunk",
		"created": s.createdSec,
		"model":   s.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": nil,
			},
		},
	}
	s.chunkIndex++
	body, _ := jsonMarshal(chunk)
	return append(append([]byte("data: "), body...), '\n', '\n')
}

func (s *kiroStreamState) publishExplicitUsage() {
	if s == nil || s.reporter == nil || s.usagePublished {
		return
	}
	if s.usagePrompt <= 0 && s.usageOutput <= 0 {
		return
	}
	s.reporter.Publish(s.ctx, coreusage.Detail{
		InputTokens:  s.usagePrompt,
		OutputTokens: s.usageOutput,
		TotalTokens:  s.usagePrompt + s.usageOutput,
	})
	s.usagePublished = true
}

func (s *kiroStreamState) publishFinalUsage() {
	if s == nil || s.reporter == nil || s.usagePublished {
		return
	}
	if s.usagePrompt > 0 || s.usageOutput > 0 {
		s.publishExplicitUsage()
		return
	}
	if s.contentLen > 0 || s.contextPct > 0 {
		estOutput := s.contentLen / 4
		var estInput int64
		if s.contextPct > 0 {
			estInput = int64(s.contextPct * 200000.0 / 100.0)
		}
		s.reporter.Publish(s.ctx, coreusage.Detail{
			InputTokens:  estInput,
			OutputTokens: estOutput,
			TotalTokens:  estInput + estOutput,
		})
		s.usagePublished = true
		return
	}
	s.reporter.EnsurePublished(s.ctx)
	s.usagePublished = true
}

func (s *kiroStreamState) encodeFinish() []byte {
	finishReason := "stop"
	if s.hasToolCalls {
		finishReason = "tool_calls"
	}
	chunk := map[string]any{
		"id":      s.responseID,
		"object":  "chat.completion.chunk",
		"created": s.createdSec,
		"model":   s.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	}
	if s.usagePrompt > 0 || s.usageOutput > 0 {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     s.usagePrompt,
			"completion_tokens": s.usageOutput,
			"total_tokens":      s.usagePrompt + s.usageOutput,
		}
	} else if s.contentLen > 0 || s.contextPct > 0 {
		// Estimate when upstream did not provide explicit tokens.
		estOutput := s.contentLen / 4
		var estInput int64
		if s.contextPct > 0 {
			estInput = int64(s.contextPct * 200000.0 / 100.0)
		}
		chunk["usage"] = map[string]any{
			"prompt_tokens":     estInput,
			"completion_tokens": estOutput,
			"total_tokens":      estInput + estOutput,
		}
	}
	s.finishEmitted = true
	body, _ := jsonMarshal(chunk)
	return append(append([]byte("data: "), body...), '\n', '\n')
}

func (s *kiroStreamState) flush() [][]byte {
	out := [][]byte{}
	if !s.finishEmitted {
		out = append(out, s.encodeFinish())
	}
	s.publishFinalUsage()
	out = append(out, []byte("data: [DONE]\n\n"))
	return out
}

// kiroNonStreamAggregator collapses streaming SSE chunks back into a single
// non-stream OpenAI Chat Completions response for callers that asked for
// non-stream output.
type kiroNonStreamAggregator struct {
	model        string
	id           string
	created      int64
	contentParts []string
	reasoning    []string
	toolCalls    map[int]map[string]any
	toolOrder    []int
	finishReason string
	usage        map[string]any
}

func newKiroNonStreamAggregator(model string) *kiroNonStreamAggregator {
	return &kiroNonStreamAggregator{
		model:     model,
		toolCalls: map[int]map[string]any{},
	}
}

func (a *kiroNonStreamAggregator) feed(payload []byte) {
	line := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	jsonPart := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(jsonPart, []byte("[DONE]")) {
		return
	}
	if a.id == "" {
		a.id = gjson.GetBytes(jsonPart, "id").String()
	}
	if a.created == 0 {
		a.created = gjson.GetBytes(jsonPart, "created").Int()
	}
	choice := gjson.GetBytes(jsonPart, "choices.0")
	delta := choice.Get("delta")
	if c := delta.Get("content"); c.Exists() && c.Type == gjson.String {
		a.contentParts = append(a.contentParts, c.String())
	}
	if r := delta.Get("reasoning_content"); r.Exists() && r.Type == gjson.String {
		a.reasoning = append(a.reasoning, r.String())
	}
	if calls := delta.Get("tool_calls"); calls.IsArray() {
		for _, c := range calls.Array() {
			idx := int(c.Get("index").Int())
			existing, ok := a.toolCalls[idx]
			if !ok {
				existing = map[string]any{
					"index": idx,
					"id":    c.Get("id").String(),
					"type":  "function",
					"function": map[string]any{
						"name":      c.Get("function.name").String(),
						"arguments": "",
					},
				}
				a.toolCalls[idx] = existing
				a.toolOrder = append(a.toolOrder, idx)
			}
			if id := c.Get("id").String(); id != "" {
				existing["id"] = id
			}
			if name := c.Get("function.name").String(); name != "" {
				existing["function"].(map[string]any)["name"] = name
			}
			if args := c.Get("function.arguments"); args.Exists() && args.Type == gjson.String {
				prev := existing["function"].(map[string]any)["arguments"].(string)
				existing["function"].(map[string]any)["arguments"] = prev + args.String()
			}
		}
	}
	if fr := choice.Get("finish_reason"); fr.Exists() && fr.Type == gjson.String {
		a.finishReason = fr.String()
	}
	if usage := gjson.GetBytes(jsonPart, "usage"); usage.IsObject() {
		a.usage = map[string]any{
			"prompt_tokens":     usage.Get("prompt_tokens").Int(),
			"completion_tokens": usage.Get("completion_tokens").Int(),
			"total_tokens":      usage.Get("total_tokens").Int(),
		}
	}
}

func (a *kiroNonStreamAggregator) build() []byte {
	if a.id == "" {
		a.id = "chatcmpl-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}
	if a.created == 0 {
		a.created = time.Now().Unix()
	}
	if a.finishReason == "" {
		a.finishReason = "stop"
	}
	message := map[string]any{
		"role":    "assistant",
		"content": strings.Join(a.contentParts, ""),
	}
	if len(a.reasoning) > 0 {
		message["reasoning_content"] = strings.Join(a.reasoning, "")
	}
	if len(a.toolOrder) > 0 {
		calls := make([]any, 0, len(a.toolOrder))
		for _, idx := range a.toolOrder {
			calls = append(calls, a.toolCalls[idx])
		}
		message["tool_calls"] = calls
		message["content"] = nil
	}
	resp := map[string]any{
		"id":      a.id,
		"object":  "chat.completion",
		"created": a.created,
		"model":   a.model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": a.finishReason,
			},
		},
	}
	if a.usage != nil {
		resp["usage"] = a.usage
	}
	body, _ := jsonMarshal(resp)
	return body
}

// ensureAccessToken validates the cached access_token, refreshing in place
// when missing or near expiry. Returns the bearer token to attach to the
// upstream request.
func (e *KiroExecutor) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if auth == nil {
		return "", statusErr{code: http.StatusUnauthorized, msg: "kiro executor: missing auth"}
	}
	token := kiroAccessTokenFromAuth(auth)
	exp := kiroTokenExpiry(auth.Metadata)
	if token != "" && exp.After(time.Now().Add(kiroAccessTokenSkew)) {
		return token, nil
	}
	if err := e.refreshTokenInPlace(ctx, auth); err != nil {
		return "", err
	}
	token = kiroAccessTokenFromAuth(auth)
	if token == "" {
		return "", statusErr{code: http.StatusUnauthorized, msg: "kiro executor: refresh returned empty access token"}
	}
	return token, nil
}

// refreshTokenInPlace dispatches to either AWS SSO OIDC or Kiro Social Auth
// depending on which credential fields were imported with this auth.
func (e *KiroExecutor) refreshTokenInPlace(ctx context.Context, auth *cliproxyauth.Auth) error {
	if auth == nil {
		return statusErr{code: http.StatusUnauthorized, msg: "kiro executor: missing auth"}
	}
	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	refreshToken := kiroAuthString(auth, "refresh_token", "refreshToken")
	if refreshToken == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "kiro executor: missing refresh token"}
	}

	clientID := kiroAuthString(auth, "client_id", "clientId")
	clientSecret := kiroAuthString(auth, "client_secret", "clientSecret")

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	if clientID != "" && clientSecret != "" {
		region := kiroRegionForAuth(auth)
		if !strings.EqualFold(kiroMetaString(auth.Metadata, "auth_method", "authMethod"), kiroAuthMethodIDC) {
			region = kiroDefaultRegion
		}
		return kiroRefreshAWSSSO(ctx, httpClient, region, clientID, clientSecret, refreshToken, auth)
	}
	return kiroRefreshSocial(ctx, httpClient, refreshToken, auth)
}

func kiroRefreshAWSSSO(ctx context.Context, httpClient *http.Client, region, clientID, clientSecret, refreshToken string, auth *cliproxyauth.Auth) error {
	endpoint := fmt.Sprintf(kiroAWSAuthHostFmt, region)
	body, _ := json.Marshal(map[string]any{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: close refresh response error: %v", errClose)
		}
	}()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusErr{code: resp.StatusCode, msg: "kiro AWS refresh failed: " + string(respBody)}
	}
	return kiroApplyTokenJSON(respBody, refreshToken, auth, kiroProvider+"-aws")
}

func kiroRefreshSocial(ctx context.Context, httpClient *http.Client, refreshToken string, auth *cliproxyauth.Auth) error {
	body, _ := json.Marshal(map[string]any{"refreshToken": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, kiroSocialTokenURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kiro-cli/1.0.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kiro executor: close refresh response error: %v", errClose)
		}
	}()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusErr{code: resp.StatusCode, msg: "kiro social refresh failed: " + string(respBody)}
	}
	return kiroApplyTokenJSON(respBody, refreshToken, auth, kiroProvider+"-social")
}

func kiroApplyTokenJSON(respBody []byte, fallbackRefresh string, auth *cliproxyauth.Auth, kind string) error {
	access := gjson.GetBytes(respBody, "accessToken").String()
	if access == "" {
		access = gjson.GetBytes(respBody, "access_token").String()
	}
	refresh := gjson.GetBytes(respBody, "refreshToken").String()
	if refresh == "" {
		refresh = gjson.GetBytes(respBody, "refresh_token").String()
	}
	if refresh == "" {
		refresh = fallbackRefresh
	}
	expiresIn := gjson.GetBytes(respBody, "expiresIn").Int()
	if expiresIn == 0 {
		expiresIn = gjson.GetBytes(respBody, "expires_in").Int()
	}

	if auth.Metadata == nil {
		auth.Metadata = map[string]any{}
	}
	auth.Metadata["access_token"] = access
	auth.Metadata["refresh_token"] = refresh
	if expiresIn > 0 {
		auth.Metadata["expires_in"] = expiresIn
		auth.Metadata["expired"] = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339)
	auth.Metadata["type"] = kind
	return nil
}

func kiroAuthString(auth *cliproxyauth.Auth, keys ...string) string {
	if auth == nil {
		return ""
	}
	if v := kiroMetaString(auth.Metadata, keys...); v != "" {
		return v
	}
	for _, k := range keys {
		if v := strings.TrimSpace(auth.Attributes[k]); v != "" {
			return v
		}
	}
	return ""
}

func kiroAccessTokenFromAuth(a *cliproxyauth.Auth) string {
	if a == nil {
		return ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
		if v, ok := a.Metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["access_token"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(a.Attributes["accessToken"]); v != "" {
			return v
		}
	}
	return ""
}

func kiroTokenExpiry(metadata map[string]any) time.Time {
	if metadata == nil {
		return time.Time{}
	}
	for _, key := range []string{"expired", "expires_at"} {
		if v, ok := metadata[key].(string); ok && v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t
			}
		}
	}
	if v, ok := metadata["expires_at"].(float64); ok && v > 0 {
		return time.Unix(int64(v), 0)
	}
	if v, ok := metadata["expires_in"].(float64); ok && v > 0 {
		if ref, ok2 := metadata["last_refresh"].(string); ok2 {
			if t, err := time.Parse(time.RFC3339, ref); err == nil {
				return t.Add(time.Duration(v) * time.Second)
			}
		}
		return time.Now().Add(time.Duration(v) * time.Second)
	}
	return time.Time{}
}

func kiroMetaString(metadata map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := metadata[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func kiroRegionForAuth(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return kiroDefaultRegion
	}
	if v, ok := auth.Metadata["region"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := auth.Attributes["region"]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if arn := kiroExtractProfileArn(auth.Attributes, auth.Metadata); arn != "" {
		parts := strings.Split(arn, ":")
		if len(parts) >= 4 && strings.TrimSpace(parts[3]) != "" {
			return parts[3]
		}
	}
	return kiroDefaultRegion
}

func kiroUpstreamLog(req *http.Request, body []byte, auth *cliproxyauth.Auth, provider, url string) helps.UpstreamRequestLog {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	return helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   req.Header.Clone(),
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
}

func kiroTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
