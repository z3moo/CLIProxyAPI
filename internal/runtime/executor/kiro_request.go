// Package executor provides runtime execution capabilities for various AI service providers.
// This file builds the AWS CodeWhisperer GenerateAssistantResponse payload that
// the Kiro executor sends upstream, translating an OpenAI Chat Completions
// shaped request into Kiro's conversationState/userInputMessage shape.
package executor

import (
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	kiroAgenticSuffix         = "-agentic"
	kiroThinkingSuffix        = "-thinking"
	kiroDefaultThinkingBudget = 16000

	// kiroAgenticSystemPrompt is the chunked-write protocol injected when a
	// "-agentic" model variant is requested. It mirrors 9router's prompt and
	// keeps file writes within Kiro's ~2-3 minute server timeout window.
	kiroAgenticSystemPrompt = `# CRITICAL: CHUNKED WRITE PROTOCOL (MANDATORY)

You MUST follow these rules for ALL file operations. Violation causes server timeouts and task failure.

## ABSOLUTE LIMITS
- **MAXIMUM 350 LINES** per single write/edit operation - NO EXCEPTIONS
- **RECOMMENDED 300 LINES** or less for optimal performance
- **NEVER** write entire files in one operation if >300 lines

## MANDATORY CHUNKED WRITE STRATEGY

### For NEW FILES (>300 lines total):
1. FIRST: Write initial chunk (first 250-300 lines) using write_to_file/fsWrite
2. THEN: Append remaining content in 250-300 line chunks using file append operations
3. REPEAT: Continue appending until complete

### For EDITING EXISTING FILES:
1. Use surgical edits (apply_diff/targeted edits) - change ONLY what's needed
2. NEVER rewrite entire files - use incremental modifications
3. Split large refactors into multiple small, focused edits

REMEMBER: When in doubt, write LESS per operation. Multiple small operations > one large operation.`
)

// kiroResolvedModel describes the upstream Kiro model id plus the synthetic
// suffix flags 9router uses to drive thinking and agentic behaviours.
type kiroResolvedModel struct {
	upstream string
	agentic  bool
	thinking bool
}

// resolveKiroModel strips the "-agentic" and "-thinking" 9router-style
// suffixes from a model name and reports which behaviours were requested.
func resolveKiroModel(model string) kiroResolvedModel {
	upstream := strings.TrimSpace(model)
	out := kiroResolvedModel{upstream: upstream}
	if strings.HasSuffix(out.upstream, kiroAgenticSuffix) {
		out.agentic = true
		out.upstream = strings.TrimSuffix(out.upstream, kiroAgenticSuffix)
	}
	if strings.HasSuffix(out.upstream, kiroThinkingSuffix) {
		out.thinking = true
		out.upstream = strings.TrimSuffix(out.upstream, kiroThinkingSuffix)
	}
	return out
}

// kiroIsThinkingEnabled detects whether the inbound OpenAI-shaped body asks
// for reasoning/thinking. Mirrors the 9router heuristic: Anthropic-style
// `thinking.type=enabled`, OpenAI `reasoning_effort` / `reasoning.effort`,
// or an explicit `<thinking_mode>enabled</thinking_mode>` tag in messages.
func kiroIsThinkingEnabled(body []byte, model string) bool {
	if v := gjson.GetBytes(body, "thinking.type"); v.Exists() && strings.EqualFold(v.String(), "enabled") {
		budget := gjson.GetBytes(body, "thinking.budget_tokens")
		if !budget.Exists() || budget.Int() > 0 {
			return true
		}
	}
	for _, path := range []string{"reasoning_effort", "reasoning.effort"} {
		if v := gjson.GetBytes(body, path); v.Exists() {
			eff := strings.ToLower(strings.TrimSpace(v.String()))
			if eff == "low" || eff == "medium" || eff == "high" || eff == "auto" {
				return true
			}
		}
	}
	if kiroMessagesContainThinkingTag(body) {
		return true
	}
	if model != "" {
		lower := strings.ToLower(model)
		if strings.Contains(lower, "thinking") || strings.Contains(lower, "-reason") {
			return true
		}
	}
	return false
}

func kiroMessagesContainThinkingTag(body []byte) bool {
	const tagEnabled = "<thinking_mode>enabled</thinking_mode>"
	const tagInterleaved = "<thinking_mode>interleaved</thinking_mode>"
	if v := gjson.GetBytes(body, "system"); v.Exists() && v.Type == gjson.String {
		s := v.String()
		if strings.Contains(s, tagEnabled) || strings.Contains(s, tagInterleaved) {
			return true
		}
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return false
	}
	for _, msg := range messages.Array() {
		role := msg.Get("role").String()
		if role != "system" && role != "user" {
			continue
		}
		content := msg.Get("content")
		switch {
		case content.Type == gjson.String:
			s := content.String()
			if strings.Contains(s, tagEnabled) || strings.Contains(s, tagInterleaved) {
				return true
			}
		case content.IsArray():
			for _, part := range content.Array() {
				s := part.Get("text").String()
				if strings.Contains(s, tagEnabled) || strings.Contains(s, tagInterleaved) {
					return true
				}
			}
		}
	}
	return false
}

func kiroBuildThinkingPrefix(budget int) string {
	if budget <= 0 || budget > 32000 {
		budget = kiroDefaultThinkingBudget
	}
	return "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>" +
		intToString(budget) + "</max_thinking_length>"
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}

// kiroToolSpec is the inputSchema-bearing tool entry attached to the first
// user message in the Kiro history.
type kiroToolSpec struct {
	ToolSpecification map[string]any `json:"toolSpecification"`
}

// kiroPayloadInput is the executor-friendly summary returned alongside the
// payload bytes; it carries the upstream model id used for routing.
type kiroPayloadInput struct {
	UpstreamModel string
	Body          []byte
}

// buildKiroRequestPayload converts an OpenAI Chat Completions body into the
// AWS CodeWhisperer GenerateAssistantResponse payload that Kiro expects.
//
// The translation rules mirror 9router's openai-to-kiro:
//   - system / tool roles are folded into adjacent user messages
//   - tool_use / tool_result blocks are mapped into Kiro's toolUses /
//     userInputMessageContext.toolResults
//   - tools are attached to the currentMessage's userInputMessageContext
//   - the current user message gets a system-prompt prefix that injects
//     timestamp + optional thinking_mode + optional agentic protocol
//
// Image content (image_url / Claude image blocks) is translated when present.
func buildKiroRequestPayload(model string, body []byte, profileArn string) kiroPayloadInput {
	resolved := resolveKiroModel(model)
	thinkingEnabled := resolved.thinking || kiroIsThinkingEnabled(body, model)
	upstream := resolved.upstream

	messages := gjson.GetBytes(body, "messages").Array()
	tools := gjson.GetBytes(body, "tools").Array()

	type pendingState struct {
		role         string
		userText     []string
		assistantTxt []string
		toolResults  []map[string]any
		images       []map[string]any
	}
	history := []map[string]any{}
	current := pendingState{}

	flushPending := func(p *pendingState) {
		switch p.role {
		case "user":
			content := strings.TrimSpace(strings.Join(p.userText, "\n\n"))
			if content == "" {
				content = "continue"
			}
			userMsg := map[string]any{
				"content": content,
				"modelId": "",
			}
			if len(p.images) > 0 {
				userMsg["images"] = p.images
			}
			ctx := map[string]any{}
			if len(p.toolResults) > 0 {
				ctx["toolResults"] = p.toolResults
			}
			// First user message carries the tools advertisement.
			if len(history) == 0 && len(tools) > 0 {
				ctx["tools"] = buildKiroToolSpecs(tools)
			}
			if len(ctx) > 0 {
				userMsg["userInputMessageContext"] = ctx
			}
			history = append(history, map[string]any{"userInputMessage": userMsg})
		case "assistant":
			content := strings.TrimSpace(strings.Join(p.assistantTxt, "\n\n"))
			if content == "" {
				content = "..."
			}
			history = append(history, map[string]any{
				"assistantResponseMessage": map[string]any{"content": content},
			})
		}
		*p = pendingState{}
	}

	for _, msg := range messages {
		role := msg.Get("role").String()
		if role == "system" || role == "tool" {
			role = "user"
		}
		if current.role != "" && role != current.role {
			flushPending(&current)
		}
		current.role = role

		switch role {
		case "user":
			textParts, images, toolResults := extractKiroUserContent(msg)
			if msg.Get("role").String() == "tool" {
				toolID := msg.Get("tool_call_id").String()
				toolText := msg.Get("content").String()
				current.toolResults = append(current.toolResults, map[string]any{
					"toolUseId": toolID,
					"status":    "success",
					"content":   []map[string]any{{"text": toolText}},
				})
			}
			if len(textParts) > 0 {
				current.userText = append(current.userText, strings.Join(textParts, "\n"))
			}
			current.images = append(current.images, images...)
			current.toolResults = append(current.toolResults, toolResults...)
		case "assistant":
			text, toolUses := extractKiroAssistantContent(msg)
			if text != "" {
				current.assistantTxt = append(current.assistantTxt, text)
			}
			if len(toolUses) > 0 {
				flushPending(&current)
				if len(history) > 0 {
					last := history[len(history)-1]
					if arm, ok := last["assistantResponseMessage"].(map[string]any); ok {
						arm["toolUses"] = toolUses
					}
				}
			}
		}
	}
	if current.role != "" {
		flushPending(&current)
	}

	// Pop the trailing user message as currentMessage; if none, synthesize one.
	var currentMsg map[string]any
	for i := len(history) - 1; i >= 0; i-- {
		if um, ok := history[i]["userInputMessage"].(map[string]any); ok {
			currentMsg = um
			history = append(history[:i], history[i+1:]...)
			break
		}
	}
	if currentMsg == nil {
		currentMsg = map[string]any{"content": "continue", "modelId": ""}
	}

	// Capture the tools list before we strip it from the leading history entry.
	var firstHistoryTools []kiroToolSpec
	if len(history) > 0 {
		if um, ok := history[0]["userInputMessage"].(map[string]any); ok {
			if ctx, ok2 := um["userInputMessageContext"].(map[string]any); ok2 {
				if t, ok3 := ctx["tools"].([]kiroToolSpec); ok3 {
					firstHistoryTools = t
				}
			}
		}
	}

	// Cleanup: drop tools from history; ensure each history user message has
	// modelId set; collapse empty userInputMessageContext.
	for _, item := range history {
		if um, ok := item["userInputMessage"].(map[string]any); ok {
			if ctx, ok2 := um["userInputMessageContext"].(map[string]any); ok2 {
				delete(ctx, "tools")
				if len(ctx) == 0 {
					delete(um, "userInputMessageContext")
				}
			}
			if v, _ := um["modelId"].(string); v == "" {
				um["modelId"] = upstream
			}
		}
	}

	// Merge consecutive user messages — Kiro requires alternating user/assistant.
	merged := make([]map[string]any, 0, len(history))
	for _, item := range history {
		if um, ok := item["userInputMessage"].(map[string]any); ok && len(merged) > 0 {
			if prev, ok2 := merged[len(merged)-1]["userInputMessage"].(map[string]any); ok2 {
				prev["content"] = prev["content"].(string) + "\n\n" + um["content"].(string)
				continue
			}
		}
		merged = append(merged, item)
	}

	// Build the final user-visible content with the standard prefix block.
	prefix := []string{}
	if thinkingEnabled {
		prefix = append(prefix, kiroBuildThinkingPrefix(kiroDefaultThinkingBudget))
	}
	prefix = append(prefix, "[Context: Current time is "+kiroTimestamp()+"]")
	if resolved.agentic {
		prefix = append(prefix, kiroAgenticSystemPrompt)
	}
	finalContent := strings.Join(prefix, "\n\n") + "\n\n" + currentMsg["content"].(string)

	currentUserInput := map[string]any{
		"content": finalContent,
		"modelId": upstream,
		"origin":  "AI_EDITOR",
	}
	if imgs, ok := currentMsg["images"].([]map[string]any); ok && len(imgs) > 0 {
		currentUserInput["images"] = imgs
	}
	if ctx, ok := currentMsg["userInputMessageContext"].(map[string]any); ok && len(ctx) > 0 {
		currentUserInput["userInputMessageContext"] = ctx
	}
	// If history carried tools (i.e., there were prior user turns), forward
	// them onto the current message so Kiro still sees the tool catalog.
	if len(firstHistoryTools) > 0 {
		ctx, _ := currentUserInput["userInputMessageContext"].(map[string]any)
		if ctx == nil {
			ctx = map[string]any{}
			currentUserInput["userInputMessageContext"] = ctx
		}
		if _, exists := ctx["tools"]; !exists {
			ctx["tools"] = firstHistoryTools
		}
	}

	conversationState := map[string]any{
		"chatTriggerType": "MANUAL",
		"conversationId":  uuid.NewString(),
		"currentMessage":  map[string]any{"userInputMessage": currentUserInput},
		"history":         merged,
	}
	root := map[string]any{"conversationState": conversationState}
	if profileArn != "" {
		root["profileArn"] = profileArn
	}

	// Inference config: pull temperature / top_p when present, cap maxTokens.
	infer := map[string]any{"maxTokens": 32000}
	if v := gjson.GetBytes(body, "temperature"); v.Exists() {
		infer["temperature"] = v.Float()
	}
	if v := gjson.GetBytes(body, "top_p"); v.Exists() {
		infer["topP"] = v.Float()
	}
	root["inferenceConfig"] = infer

	out, _ := jsonMarshalIndentNone(root)
	return kiroPayloadInput{UpstreamModel: upstream, Body: out}
}

// extractKiroUserContent pulls plain text, image blocks, and Claude-style
// tool_result blocks out of an OpenAI/Claude shaped user message.
func extractKiroUserContent(msg gjson.Result) (textParts []string, images []map[string]any, toolResults []map[string]any) {
	content := msg.Get("content")
	switch {
	case content.Type == gjson.String:
		if s := content.String(); s != "" {
			textParts = append(textParts, s)
		}
	case content.IsArray():
		for _, part := range content.Array() {
			ptype := part.Get("type").String()
			switch ptype {
			case "text":
				textParts = append(textParts, part.Get("text").String())
			case "image_url":
				url := part.Get("image_url.url").String()
				if img, ok := decodeKiroDataURIImage(url); ok {
					images = append(images, img)
				} else if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
					textParts = append(textParts, "[Image: "+url+"]")
				}
			case "image":
				if part.Get("source.type").String() == "base64" {
					mediaType := part.Get("source.media_type").String()
					if mediaType == "" {
						mediaType = "image/png"
					}
					format := mediaType
					if i := strings.Index(format, "/"); i >= 0 {
						format = format[i+1:]
					}
					images = append(images, map[string]any{
						"format": format,
						"source": map[string]any{"bytes": part.Get("source.data").String()},
					})
				}
			case "tool_result":
				text := ""
				inner := part.Get("content")
				if inner.IsArray() {
					var pieces []string
					for _, c := range inner.Array() {
						pieces = append(pieces, c.Get("text").String())
					}
					text = strings.Join(pieces, "\n")
				} else if inner.Type == gjson.String {
					text = inner.String()
				}
				toolResults = append(toolResults, map[string]any{
					"toolUseId": part.Get("tool_use_id").String(),
					"status":    "success",
					"content":   []map[string]any{{"text": text}},
				})
			default:
				if t := part.Get("text"); t.Exists() {
					textParts = append(textParts, t.String())
				}
			}
		}
	}
	return
}

// extractKiroAssistantContent pulls assistant text and tool calls out of an
// OpenAI Chat Completions or Claude messages shaped assistant message.
func extractKiroAssistantContent(msg gjson.Result) (string, []map[string]any) {
	var textParts []string
	var toolUses []map[string]any

	content := msg.Get("content")
	if content.Type == gjson.String {
		textParts = append(textParts, content.String())
	} else if content.IsArray() {
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text":
				textParts = append(textParts, part.Get("text").String())
			case "tool_use":
				input := part.Get("input")
				var inputAny any = map[string]any{}
				if input.Exists() && input.Raw != "" {
					inputAny = gjsonRawToAny(input)
				}
				toolUses = append(toolUses, map[string]any{
					"toolUseId": part.Get("id").String(),
					"name":      part.Get("name").String(),
					"input":     inputAny,
				})
			}
		}
	}

	if calls := msg.Get("tool_calls"); calls.IsArray() {
		toolUses = nil
		for _, c := range calls.Array() {
			id := c.Get("id").String()
			if id == "" {
				id = uuid.NewString()
			}
			args := c.Get("function.arguments")
			var inputAny any = map[string]any{}
			if args.Exists() {
				switch args.Type {
				case gjson.String:
					inputAny = parseKiroToolArguments(args.String())
				default:
					inputAny = gjsonRawToAny(args)
				}
			}
			toolUses = append(toolUses, map[string]any{
				"toolUseId": id,
				"name":      c.Get("function.name").String(),
				"input":     inputAny,
			})
		}
	}

	return strings.TrimSpace(strings.Join(textParts, "\n")), toolUses
}

// buildKiroToolSpecs turns OpenAI / Claude tool descriptors into Kiro
// toolSpecification entries. Kiro requires inputSchema.json with a non-nil
// required array, even when empty.
func buildKiroToolSpecs(tools []gjson.Result) []kiroToolSpec {
	out := make([]kiroToolSpec, 0, len(tools))
	for _, t := range tools {
		name := t.Get("function.name").String()
		if name == "" {
			name = t.Get("name").String()
		}
		desc := t.Get("function.description").String()
		if desc == "" {
			desc = t.Get("description").String()
		}
		if strings.TrimSpace(desc) == "" {
			desc = "Tool: " + name
		}
		schemaRaw := t.Get("function.parameters")
		if !schemaRaw.Exists() {
			schemaRaw = t.Get("parameters")
		}
		if !schemaRaw.Exists() {
			schemaRaw = t.Get("input_schema")
		}
		schema := map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}
		if schemaRaw.Exists() && schemaRaw.IsObject() {
			schema = gjsonRawToAny(schemaRaw).(map[string]any)
			if _, ok := schema["required"]; !ok {
				schema["required"] = []any{}
			}
		}
		out = append(out, kiroToolSpec{
			ToolSpecification: map[string]any{
				"name":        name,
				"description": desc,
				"inputSchema": map[string]any{"json": schema},
			},
		})
	}
	return out
}

func decodeKiroDataURIImage(uri string) (map[string]any, bool) {
	if !strings.HasPrefix(uri, "data:") {
		return nil, false
	}
	commaIdx := strings.Index(uri, ",")
	if commaIdx < 0 {
		return nil, false
	}
	header := uri[5:commaIdx]
	data := uri[commaIdx+1:]
	mediaType := header
	if semi := strings.Index(header, ";"); semi >= 0 {
		mediaType = header[:semi]
	}
	format := mediaType
	if i := strings.Index(format, "/"); i >= 0 {
		format = format[i+1:]
	}
	return map[string]any{
		"format": format,
		"source": map[string]any{"bytes": data},
	}, true
}

// parseKiroToolArguments accepts OpenAI tool_call.function.arguments which is
// JSON-encoded as a string and tries to decode it; falls back to {} on error.
func parseKiroToolArguments(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return map[string]any{}
	}
	val := gjson.Parse(raw)
	if !val.Exists() {
		return map[string]any{}
	}
	return gjsonRawToAny(val)
}

// gjsonRawToAny converts a gjson.Result into Go native types (map / slice /
// primitive) by re-parsing its raw JSON with sjson-friendly rules.
func gjsonRawToAny(r gjson.Result) any {
	switch r.Type {
	case gjson.String:
		return r.String()
	case gjson.Number:
		// Preserve integer values when possible.
		if r.Num == float64(int64(r.Num)) {
			return int64(r.Num)
		}
		return r.Num
	case gjson.True:
		return true
	case gjson.False:
		return false
	case gjson.Null:
		return nil
	}
	if r.IsArray() {
		out := make([]any, 0)
		for _, item := range r.Array() {
			out = append(out, gjsonRawToAny(item))
		}
		return out
	}
	if r.IsObject() {
		out := map[string]any{}
		r.ForEach(func(key, value gjson.Result) bool {
			out[key.String()] = gjsonRawToAny(value)
			return true
		})
		return out
	}
	return nil
}

// kiroExtractProfileArn pulls the optional profileArn out of auth attributes
// or metadata. Both shapes are accepted because OAuth flows tend to land
// values in metadata while operator imports use attributes.
func kiroExtractProfileArn(attributes map[string]string, metadata map[string]any) string {
	if v, ok := attributes["profile_arn"]; ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["profile_arn"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["profileArn"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

// jsonMarshalIndentNone is a thin wrapper that keeps the body compact.
func jsonMarshalIndentNone(v any) ([]byte, error) {
	return jsonMarshal(v)
}

// kiroSetUpstreamModel rewrites the body so the upstream "model" field
// matches the resolved Kiro model. Useful when the OpenAI body still carries
// a synthetic alias from the inbound request.
func kiroSetUpstreamModel(body []byte, model string) []byte {
	if len(body) == 0 || model == "" {
		return body
	}
	updated, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return body
	}
	return updated
}
