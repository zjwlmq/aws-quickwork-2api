package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type chatTurn struct {
	server      *Server
	row         *store.ChatSession
	history     []Message
	contextHash string
	toolSchemas map[string]map[string]any
	deltaStart  int
	resumed     bool
	dispatched  bool
	accountUsed bool
	observation chatReplyObserver
}

func (s *Server) beginChatTurn(ctx context.Context, req *ChatRequest, scope, owner, model, system, toolPrompt string) (*chatTurn, error) {
	history := conversationMessages(req.Messages)
	if len(history) == 0 {
		return nil, &upstreamRequestError{code: "invalid_request_error", message: "messages contain no conversation"}
	}
	contextHash := diagnostics.Fingerprint(model + "\x00" + system + "\x00" + toolPrompt)
	// Caller thread identifiers are scoped aliases, never native thread IDs. The
	// same identifier on another API key cannot expose or append to this history.
	rawScope := scope
	scope = diagnostics.Fingerprint(scope)
	if s.store != nil && strings.HasPrefix(rawScope, "chat_") {
		if parent, err := s.store.GetChatSession(ctx, owner, rawScope); err == nil {
			scope = parent.Scope
		}
	}
	schemas := chatToolSchemas(req)
	prefixes := transcriptPrefixes(contextHash, canonicalToolHistory(history, schemas))
	t := &chatTurn{server: s, history: history, contextHash: contextHash, toolSchemas: schemas}
	if s.store == nil {
		return t, nil
	}
	candidates, err := s.store.ChatCandidates(ctx, owner, scope, model, contextHash)
	if err != nil {
		return nil, err
	}
	for _, row := range candidates {
		if row.MessageCount <= 0 || row.MessageCount >= int64(len(history)) || prefixes[row.MessageCount-1] != row.PrefixHash {
			continue
		}
		claimed, err := s.store.ClaimChatSession(ctx, row.ID, row.Version)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue
		}
		account, err := s.accounts.GetFresh(ctx, row.AccountID)
		if err != nil || account.Disabled {
			_ = s.store.RetireChatSession(ctx, row.ID)
			diagnostics.Event(ctx, "warn", "chat.session.account_unavailable", "chat_session_id", row.ID, "account_id", row.AccountID, "error", err)
			continue
		}
		t.row, t.deltaStart, t.resumed = &row, int(row.MessageCount), true
		break
	}
	if t.row == nil {
		t.row = &store.ChatSession{ID: "chat_" + strings.ReplaceAll(uuid(), "-", ""), Owner: owner, Scope: scope, Model: model, ContextHash: contextHash, ThreadID: uuid()}
		if err := s.store.CreateChatSession(ctx, t.row); err != nil {
			return nil, err
		}
	}
	diagnostics.Event(ctx, "info", "chat.session.prepared", "chat_session_id", t.row.ID, "upstream_thread_id", t.row.ThreadID, "resumed", t.resumed, "history_messages", len(history), "reused_messages", t.deltaStart, "new_messages", len(history)-t.deltaStart, "context_sha256", contextHash)
	s.scheduleChatCleanup()
	return t, nil
}

func (t *chatTurn) delta() []Message {
	return withToolResultNames(t.history[t.deltaStart:], t.history)
}

func (t *chatTurn) account(ctx context.Context) (*store.Account, error) {
	if t.resumed && t.row != nil {
		a, err := t.server.accounts.GetFresh(ctx, t.row.AccountID)
		if err != nil {
			return nil, err
		}
		if a.Disabled {
			return nil, errors.New("bound upstream account is disabled")
		}
		if !t.accountUsed {
			if err := t.server.store.MarkChatAccountUsed(ctx, a.ID); err != nil {
				return nil, err
			}
			t.accountUsed = true
		}
		diagnostics.Event(ctx, "info", "account.selected", "account_id", a.ID, "provider", a.Provider, "expires_at", a.ExpiresAt, "pinned", true, "chat_session_id", t.row.ID)
		return a, nil
	}
	a, err := t.server.accounts.Next(ctx)
	if err != nil || t.row == nil {
		return a, err
	}
	// A pre-dispatch dial failure may safely select another account, but it must
	// also receive a new native thread. Once written, no automatic replay occurs.
	if t.row.AccountID != 0 {
		t.row.ThreadID = uuid()
	}
	t.row.AccountID = a.ID
	if err := t.server.store.BindChatSession(ctx, t.row.ID, t.row.ThreadID, a.ID); err != nil {
		return nil, err
	}
	return a, nil
}

func (t *chatTurn) bind(in *ChatInput, w http.ResponseWriter) {
	if t.row == nil {
		return
	}
	in.ThreadID, in.KeepSession, in.ResumeSession = t.row.ThreadID, true, t.resumed
	in.OnDispatch = func() { t.dispatched = true }
	w.Header().Set("X-Thread-Id", t.row.ID)
}

func (t *chatTurn) finish(requestCtx context.Context) {
	if t.row == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if requestCtx.Err() == nil && t.observation.completed && !t.observation.failed && t.dispatched {
		message := t.observation.message()
		prefixes := transcriptPrefixes(t.contextHash, canonicalToolHistory(append(append([]Message(nil), t.history...), message), t.toolSchemas))
		err = t.server.store.CompleteChatSession(ctx, t.row.ID, prefixes[len(prefixes)-1], int64(len(t.history)+1))
		diagnostics.Event(requestCtx, "info", "chat.session.committed", "chat_session_id", t.row.ID, "upstream_thread_id", t.row.ThreadID, "account_id", t.row.AccountID, "message_count", len(t.history)+1, "prefix_sha256", prefixes[len(prefixes)-1], "error", err)
	} else {
		// Unknown post-dispatch state is never reused. A later client retry or
		// branch bootstraps from the caller's last confirmed history instead.
		err = t.server.store.ReleaseChatSession(ctx, t.row.ID, t.resumed && !t.dispatched)
		diagnostics.Event(requestCtx, "warn", "chat.session.released", "chat_session_id", t.row.ID, "dispatched", t.dispatched, "reusable", t.resumed && !t.dispatched, "error", err)
	}
	if err != nil {
		_ = t.server.store.RetireChatSession(ctx, t.row.ID)
	}
}

// The observer sees OpenAI output before the Anthropic translator. It commits
// exactly the assistant message delivered to the caller, including generated
// tool IDs, and refuses incomplete/error SSE or failed writes.
type chatReplyObserver struct {
	stream, failed, completed bool
	status                    int
	pending, text             strings.Builder
	calls                     map[int]ToolCall
	finish                    string
}

func (o *chatReplyObserver) message() Message {
	m := Message{Role: "assistant", Content: o.text.String()}
	for i := 0; i < len(o.calls); i++ {
		if c, ok := o.calls[i]; ok {
			m.ToolCalls = append(m.ToolCalls, c)
		}
	}
	return m
}

func (o *chatReplyObserver) accept(data []byte) {
	var value struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Message *Message `json:"message"`
			Delta   struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int `json:"index"`
					ID, Type string
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &value) != nil {
		o.failed = true
		return
	}
	if len(value.Error) != 0 && string(value.Error) != "null" {
		o.failed = true
	}
	for _, c := range value.Choices {
		if c.Message != nil {
			o.text.WriteString(contentText(c.Message.Content))
			o.calls = make(map[int]ToolCall)
			for i, call := range c.Message.ToolCalls {
				o.calls[i] = call
			}
			o.completed = c.FinishReason != ""
		} else {
			o.text.WriteString(c.Delta.Content)
			if o.calls == nil {
				o.calls = make(map[int]ToolCall)
			}
			for _, call := range c.Delta.ToolCalls {
				t := o.calls[call.Index]
				if call.ID != "" {
					t.ID = call.ID
				}
				if call.Type != "" {
					t.Type = call.Type
				}
				t.Function.Name += call.Function.Name
				// Missing SSE arguments are empty; never apply the normal JSON
				// function-call decoder's default "{}" to an absent fragment.
				t.Function.Arguments += call.Function.Arguments
				o.calls[call.Index] = t
			}
			if c.FinishReason != "" {
				o.finish = c.FinishReason
			}
		}
	}
}

type chatObservedWriter struct {
	http.ResponseWriter
	o *chatReplyObserver
}

func (w *chatObservedWriter) WriteHeader(status int) {
	if w.o.status == 0 {
		w.o.status = status
	}
	if status != http.StatusOK {
		w.o.failed = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *chatObservedWriter) Write(b []byte) (int, error) {
	if w.o.status == 0 {
		w.o.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		w.o.failed = true
	}
	if !w.o.stream {
		w.o.pending.Write(b[:n])
	} else {
		w.o.pending.Write(b[:n])
		buffer := w.o.pending.String()
		for {
			end := strings.IndexByte(buffer, '\n')
			if end < 0 {
				break
			}
			line := strings.TrimSpace(buffer[:end])
			buffer = buffer[end+1:]
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				w.o.completed = w.o.finish != ""
				continue
			}
			w.o.accept([]byte(data))
		}
		w.o.pending.Reset()
		w.o.pending.WriteString(buffer)
	}
	return n, err
}

type chatObservedFlusher struct{ *chatObservedWriter }

func (w *chatObservedFlusher) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func observeChatWriter(w http.ResponseWriter, o *chatReplyObserver, stream bool) http.ResponseWriter {
	o.stream = stream
	base := &chatObservedWriter{ResponseWriter: w, o: o}
	if _, ok := w.(http.Flusher); ok {
		return &chatObservedFlusher{base}
	}
	return base
}

func (t *chatTurn) finishObserved(ctx context.Context) {
	if !t.observation.stream && t.observation.pending.Len() > 0 {
		t.observation.accept([]byte(t.observation.pending.String()))
	}
	if t.observation.text.Len() == 0 && len(t.observation.calls) == 0 {
		t.observation.failed = true
	}
	t.finish(ctx)
}

func (s *Server) scheduleChatCleanup() {
	if s.store == nil || !s.chatCleanupRunning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.chatCleanupRunning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		rows, err := s.store.StaleChatSessions(ctx, 4)
		if err != nil {
			return
		}
		for _, row := range rows {
			if row.AccountID > 0 {
				a, err := s.accounts.GetFresh(ctx, row.AccountID)
				if err != nil {
					continue
				}
				if err = s.upstream.DeleteSession(ctx, SessionFromAccount(a, s.cfg), row.ThreadID); err != nil {
					continue
				}
			}
			_ = s.store.DeleteChatSession(ctx, row.ID)
		}
	}()
}

func chatPrepareError(w http.ResponseWriter, err error) {
	status, code := chatErrorStatus(err)
	if status == http.StatusBadGateway {
		status, code = http.StatusInternalServerError, "session_state_error"
	}
	writeOpenAIError(w, status, code, fmt.Sprint(err))
}
