package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type Server struct {
	cfg                     *config.Config
	accounts                *accounts.Manager
	tokens                  *APIKeyAuth
	upstream                *Upstream
	store                   *store.Store
	mediaSecret             []byte
	videoSlots              chan struct{}
	chatCleanupRunning      atomic.Bool
	streamKeepAliveInterval time.Duration
	artifactRetryDelay      time.Duration
}

func NewServer(cfg *config.Config, mgr *accounts.Manager, stores ...*store.Store) *Server {
	s := &Server{
		cfg:                     cfg,
		accounts:                mgr,
		tokens:                  NewAPIKeyAuth(cfg.APIKeys),
		upstream:                NewUpstream(cfg),
		streamKeepAliveInterval: 15 * time.Second,
		artifactRetryDelay:      time.Second,
	}
	if len(stores) > 0 {
		s.store = stores[0]
	}
	s.videoSlots = make(chan struct{}, 2)
	return s
}

func (s *Server) InitMedia(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	if err := s.store.RecoverChatSessions(ctx); err != nil {
		return err
	}
	if err := s.store.InitMedia(ctx); err != nil {
		return err
	}
	secret, err := s.store.MediaSecret(ctx)
	if err != nil {
		return err
	}
	s.mediaSecret = secret
	return s.store.RecoverVideos(ctx)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/messages", s.handleAnthropic)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropic)
	mux.HandleFunc("/v1/media/{id}", s.serveMedia)
	mux.HandleFunc("/v1/videos", s.videoAPI)
	mux.HandleFunc("/v1/videos/{id}", s.videoAPI)
	mux.HandleFunc("/v1/videos/{id}/content", s.videoAPI)
	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Thread-Id, Anthropic-Version, Anthropic-Beta, Anthropic-Dangerous-Direct-Browser-Access")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID, request-id, X-Thread-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "quickwork-openai"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(s.cfg.Models))
	for _, id := range s.cfg.Models {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "quickwork",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}

	var req ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err := dec.Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.N > 1 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "n > 1 is not supported")
		return
	}
	logChatRequest(r.Context(), &req)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout.Duration())
	defer cancel()
	systemPromptText := systemPrompt(req.Messages)
	model := s.mapModel(req.Model)
	allowedTools := requestedToolNames(&req)
	choiceMode, choiceName := parseToolChoice(req.ToolChoice)
	if choiceMode == "function" && choiceName != "" {
		allowedTools[choiceName] = true
	}
	parallelTools := true
	if req.ParallelToolCalls != nil {
		parallelTools = *req.ParallelToolCalls
	}
	bridgePrompt := buildToolBridgePrompt(req.Tools, req.Functions, choiceMode, choiceName, parallelTools)
	bridgeActive := bridgePrompt != ""
	bridgeAllowed := allowedTools
	if choiceMode == "function" && choiceName != "" {
		bridgeAllowed = map[string]bool{choiceName: true}
	}

	threadID := strings.TrimSpace(r.Header.Get("X-Thread-Id"))
	turn, err := s.beginChatTurn(ctx, &req, threadID, ownerKey(tokenFromRequest(r)), model, systemPromptText, bridgePrompt)
	if err != nil {
		chatPrepareError(w, err)
		return
	}
	w = observeChatWriter(w, &turn.observation, req.Stream)
	defer turn.finishObserved(ctx)
	if turn.row != nil {
		threadID = turn.row.ID
		w.Header().Set("X-Thread-Id", threadID)
	}
	prepared, attachments, err := prepareMedia(ctx, turn.delta())
	if err != nil {
		diagnostics.Event(ctx, "warn", "chat.media.prepare_failed", "error", err)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_media", err.Error())
		return
	}
	prompt, err := formatPrompt(prepared)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	id := "chatcmpl-" + strings.ReplaceAll(uuid(), "-", "")
	created := time.Now().Unix()
	in := ChatInput{
		Prompt:       prompt,
		Model:        model,
		ThreadID:     threadID,
		Title:        titleFor(lastUserText(req.Messages)),
		SystemPrompt: systemPromptText,
		ToolPrompt:   bridgePrompt,
		Attachments:  attachments,
	}
	ctx = diagnostics.With(ctx, "completion_id", id)
	logPreparedContext(ctx, in, bridgeActive, choiceMode, choiceName)
	in, err = prepareNativeContext(ctx, in)
	if err != nil {
		diagnostics.Event(ctx, "warn", "chat.context.prepare_failed", "error", err)
		writeOpenAIError(w, http.StatusBadRequest, "context_length_exceeded", err.Error())
		return
	}

	if !req.Stream {
		var res ChatResult
		var lastErr error
		for attempt := range s.maxAttempts(ctx) {
			attemptCtx := diagnostics.With(ctx, "attempt", attempt+1)
			diagnostics.Event(attemptCtx, "info", "chat.attempt.started")
			acct, err := turn.account(attemptCtx)
			if err != nil {
				diagnostics.Event(attemptCtx, "error", "chat.attempt.account_failed", "error", err)
				lastErr = err
				break
			}
			sess := SessionFromAccount(acct, s.cfg)
			turn.bind(&in, w)
			owner := ownerKey(tokenFromRequest(r))
			in.ArtifactHandler = func(ctx context.Context, thread string, refs []NativeArtifact) ([]store.MediaFile, error) {
				return s.cacheArtifacts(ctx, sess, owner, thread, chatArtifacts(ctx, refs, bridgeActive))
			}
			if err := sess.validate(); err != nil {
				s.accounts.ReportFailure(attemptCtx, acct.ID, err)
				lastErr = err
				continue
			}
			res, err = s.upstream.Chat(attemptCtx, sess, in, nil)
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
			if requestRefused(err) || turn.dispatched {
				break
			}
			s.accounts.ReportFailure(attemptCtx, acct.ID, err)
			if ctx.Err() != nil {
				break
			}
		}
		if lastErr != nil {
			lastErr = s.chatFailure(ctx, lastErr)
			status, code := chatErrorStatus(lastErr)
			writeOpenAIError(w, status, code, lastErr.Error())
			return
		}
		text := replaceNativeLinks(res.Text, res.Files)
		var bridgeReplies []ToolCallReply
		if bridgeActive {
			text, bridgeReplies = parseClientToolCalls(text, bridgeAllowed)
			if !parallelTools && len(bridgeReplies) > 1 {
				bridgeReplies = bridgeReplies[:1]
			}
		}
		toolCalls := replyToolCalls(bridgeReplies)
		if len(toolCalls) == 0 {
			toolCalls = filterToolCalls(res.ToolCalls, allowedTools, choiceMode, choiceName, parallelTools)
		}
		for _, tc := range toolCalls {
			logClientCall(ctx, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		if strings.TrimSpace(text) == "" && len(toolCalls) == 0 && len(res.Files) == 0 {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned no text or tool calls")
			return
		}
		message := map[string]any{"role": "assistant"}
		if strings.TrimSpace(text) == "" {
			message["content"] = nil
		} else {
			message["content"] = text
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		if len(res.Files) > 0 {
			message["attachments"] = res.Files
		}
		finish := "stop"
		if len(bridgeReplies) > 0 || (strings.TrimSpace(text) == "" && len(toolCalls) > 0) {
			finish = "tool_calls"
		}
		diagnostics.Event(ctx, "info", "chat.response.completed", "stream", false, "finish_reason", finish, "text_bytes", len(text), "client_tool_calls", len(toolCalls), "files", len(res.Files), "usage", usageMap(res.Usage))
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			}},
			"usage": usageMap(res.Usage),
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unavailable", "response writer does not support flushing")
		return
	}
	started := false
	writeRaw := func(payload map[string]any) {
		body, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
		flusher.Flush()
	}
	ensureStarted := func() {
		if started {
			return
		}
		started = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		if threadID != "" {
			w.Header().Set("X-Thread-Id", threadID)
		}
		w.WriteHeader(http.StatusOK)
		writeRaw(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}},
		})
	}
	writePayload := func(payload map[string]any) {
		ensureStarted()
		writeRaw(payload)
	}
	writeChunk := func(delta map[string]any, finish any) {
		writePayload(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
	}
	writeStreamError := func(message string, code string) {
		diagnostics.Event(ctx, "error", "chat.response.stream_error", "error_message", message, "stream_started", started)
		writePayload(map[string]any{"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		}})
	}
	sawText := false
	sawTool := false
	textBytes := 0
	mediaText := mediaStream{emit: func(text string) {
		writeChunk(map[string]any{"content": text}, nil)
		if !sawText && strings.TrimSpace(text) != "" {
			sawText = true
			diagnostics.Event(ctx, "debug", "chat.response.first_content", "text_bytes", len(text), "tool_bridge", bridgeActive)
		}
		textBytes += len(text)
	}}
	finishStream := func() {
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
	failStream := func(err error) {
		status, code := chatErrorStatus(err)
		if !started {
			writeOpenAIError(w, status, code, err.Error())
			return
		}
		mediaText.push("", true, func(link string) string { return replaceNativeLinks(link, nil) })
		writeStreamError(err.Error(), code)
		finishStream()
	}
	newBridge := func() clientToolStream {
		return clientToolStream{
			allowed:   bridgeAllowed,
			checkBare: true,
			emitText:  func(text string) { mediaText.push(text, false, nil) },
		}
	}
	bridge := newBridge()
	toolIndex := 0
	toolIndexes := make(map[string]int)
	sentArgs := make(map[string]string)
	emitTool := func(tc ToolCallState) {
		if !toolAllowed(tc, allowedTools, choiceMode, choiceName) {
			return
		}
		idx, seen := toolIndexes[tc.ID]
		if !seen {
			if !parallelTools && sawTool {
				return
			}
			idx = toolIndex
			toolIndex++
			toolIndexes[tc.ID] = idx
			args := strings.TrimSpace(tc.Arguments)
			if args == "" {
				args = "{}"
			}
			sentArgs[tc.ID] = args
			logClientCall(ctx, tc.ID, tc.Name, args)
			writeChunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": args,
				},
			}}}, nil)
			sawTool = true
			return
		}
		args := strings.TrimSpace(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		prev := sentArgs[tc.ID]
		if args == prev {
			return
		}
		delta := args
		if strings.HasPrefix(args, prev) {
			delta = args[len(prev):]
		}
		sentArgs[tc.ID] = args
		if delta == "" {
			return
		}
		writeChunk(map[string]any{"tool_calls": []map[string]any{{
			"index":    idx,
			"function": map[string]any{"arguments": delta},
		}}}, nil)
		sawTool = true
	}

	var res ChatResult
	var lastErr error
	for attempt := range s.maxAttempts(ctx) {
		attemptCtx := diagnostics.With(ctx, "attempt", attempt+1)
		diagnostics.Event(attemptCtx, "info", "chat.attempt.started")
		acct, err := turn.account(attemptCtx)
		if err != nil {
			diagnostics.Event(attemptCtx, "error", "chat.attempt.account_failed", "error", err)
			lastErr = err
			break
		}
		sess := SessionFromAccount(acct, s.cfg)
		turn.bind(&in, w)
		owner := ownerKey(tokenFromRequest(r))
		in.ArtifactHandler = func(ctx context.Context, thread string, refs []NativeArtifact) ([]store.MediaFile, error) {
			return s.cacheArtifacts(ctx, sess, owner, thread, chatArtifacts(ctx, refs, bridgeActive))
		}
		if err := sess.validate(); err != nil {
			s.accounts.ReportFailure(attemptCtx, acct.ID, err)
			lastErr = err
			continue
		}
		res, err = streamWithKeepAlive(attemptCtx, s.streamKeepAliveInterval, func(callCtx context.Context, emit EventFunc) (ChatResult, error) {
			return s.upstream.Chat(callCtx, sess, in, emit)
		}, func(ev StreamEvent) error {
			switch ev.Type {
			case "text":
				if ev.Text == "" {
					return nil
				}
				if bridgeActive {
					bridge.push(ev.Text)
				} else {
					mediaText.push(ev.Text, false, nil)
				}
			case "tool_call", "tool_call_update":
				if bridgeActive {
					return nil
				}
				if ev.ToolCall != nil {
					emitTool(*ev.ToolCall)
				}
			}
			return nil
		}, func() error {
			ensureStarted()
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return err
			}
			flusher.Flush()
			diagnostics.Event(ctx, "debug", "chat.response.keepalive")
			return nil
		})
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if requestRefused(err) || turn.dispatched {
			break
		}
		s.accounts.ReportFailure(attemptCtx, acct.ID, err)
		if started || ctx.Err() != nil {
			break
		}
		// Nothing reached the client yet; reset per-attempt state and try
		// the next account in rotation.
		sawText = false
		sawTool = false
		bridge = newBridge()
		mediaText.pending = ""
		toolIndex = 0
		toolIndexes = make(map[string]int)
		sentArgs = make(map[string]string)
	}
	if lastErr != nil {
		failStream(s.chatFailure(ctx, lastErr))
		return
	}
	var replies []ToolCallReply
	if bridgeActive {
		replies, err = bridge.finish()
		if err != nil {
			failStream(err)
			return
		}
		if !parallelTools && len(replies) > 1 {
			replies = replies[:1]
		}
		for i := range replies {
			replies[i].Arguments = replaceNativeLinks(replies[i].Arguments, res.Files)
			if !json.Valid([]byte(replies[i].Arguments)) {
				failStream(fmt.Errorf("upstream returned invalid JSON arguments for client tool %q", replies[i].Name))
				return
			}
		}
	}
	mediaText.push("", true, func(link string) string { return replaceNativeLinks(link, res.Files) })
	if len(res.Files) > 0 {
		writeChunk(map[string]any{"attachments": res.Files}, nil)
	}
	for _, reply := range replies {
		emitTool(ToolCallState{ID: reply.ID, Name: reply.Name, Arguments: reply.Arguments})
	}
	if !sawText && !sawTool && len(res.Files) == 0 {
		writeStreamError("upstream returned no text or tool calls", "upstream_error")
		finishStream()
		return
	}
	finish := "stop"
	if sawTool && (bridgeActive || !sawText) {
		finish = "tool_calls"
	}
	diagnostics.Event(ctx, "info", "chat.response.completed", "stream", true, "finish_reason", finish, "text_bytes", textBytes, "client_tool_calls", toolIndex, "files", len(res.Files), "usage", usageMap(res.Usage))
	writeChunk(map[string]any{}, finish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		writePayload(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   usageMap(res.Usage),
		})
	}
	finishStream()
}

func (s *Server) chatFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("Quick 请求超过 %s 的总超时限制，回复未完成: %w", s.cfg.RequestTimeout, context.DeadlineExceeded)
		diagnostics.Event(ctx, "error", "chat.request.timed_out", "timeout_ms", s.cfg.RequestTimeout.Duration().Milliseconds(), "error", err)
	}
	return err
}

func (s *Server) maxAttempts(ctx context.Context) int {
	n, err := s.accounts.EnabledCount(ctx)
	if err != nil || n < 1 {
		return 1
	}
	max := s.cfg.MaxAttempts
	if max < 1 {
		max = 1
	}
	if n < max {
		return n
	}
	return max
}

func (s *Server) mapModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "quickwork/")
	m = strings.TrimPrefix(m, "qw-")
	switch m {
	case "smart", "fast", "balanced", "auto":
		return m
	case "":
		return s.cfg.DefaultModel
	default:
		return s.cfg.DefaultModel
	}
}

func requestedToolNames(req *ChatRequest) map[string]bool {
	names := make(map[string]bool)
	for _, t := range req.Tools {
		if name := strings.TrimSpace(t.Function.Name); name != "" {
			names[name] = true
		}
	}
	for _, f := range req.Functions {
		if name := strings.TrimSpace(f.Name); name != "" {
			names[name] = true
		}
	}
	return names
}

func parseToolChoice(v any) (mode, name string) {
	switch x := v.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(x)), ""
	case map[string]any:
		if fn, ok := x["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				return "function", strings.TrimSpace(n)
			}
		}
		if n, ok := x["name"].(string); ok {
			return "function", strings.TrimSpace(n)
		}
		if t, ok := x["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(t)), ""
		}
	}
	return "auto", ""
}

func toolAllowed(tc ToolCallState, allowed map[string]bool, mode, name string) bool {
	if mode == "none" || len(allowed) == 0 {
		return false
	}
	if !allowed[tc.Name] {
		return false
	}
	if mode == "function" && name != "" && tc.Name != name {
		return false
	}
	return true
}

func filterToolCalls(calls []ToolCallState, allowed map[string]bool, mode, name string, parallel bool) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, tc := range calls {
		if !toolAllowed(tc, allowed, mode, name) {
			continue
		}
		if !parallel && len(out) > 0 {
			break
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		toolName := strings.TrimSpace(tc.Name)
		if toolName == "" {
			toolName = "unknown"
		}
		args := strings.TrimSpace(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		out = append(out, ToolCall{
			ID:   id,
			Type: "function",
			Function: FunctionCall{
				Name:      toolName,
				Arguments: args,
			},
		})
	}
	return out
}

func usageMap(u Usage) map[string]any {
	completion := u.CompletionTokens
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens + completion
	}
	prompt := total - completion
	if prompt < 0 {
		prompt = u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
	m := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if u.CacheReadTokens > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}
