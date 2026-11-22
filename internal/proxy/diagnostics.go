package proxy

import (
	"context"
	"encoding/json"
	"sort"

	"quickwork-openai/internal/diagnostics"
)

func logChatRequest(ctx context.Context, req *ChatRequest) {
	roles := map[string]int{}
	for index, msg := range req.Messages {
		roles[msg.Role]++
		if !diagnostics.Enabled("debug") || (index >= 50 && index < len(req.Messages)-50) {
			continue
		}
		body, _ := json.Marshal(msg.Content)
		calls := make([]any, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			calls = append(calls, map[string]any{"tool_id": tc.ID, "tool_name": tc.Function.Name, "arguments_bytes": len(tc.Function.Arguments), "arguments_sha256": diagnostics.Fingerprint(tc.Function.Arguments)})
		}
		diagnostics.Event(ctx, "debug", "chat.context.message", "index", index, "role", msg.Role, "content_json_bytes", len(body), "content_sha256", diagnostics.Fingerprint(string(body)), "tool_result_id", msg.ToolCallID, "client_calls", calls)
	}
	names := requestedToolNames(req)
	toolNames := make([]string, 0, len(names))
	for name := range names {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)
	schemas, _ := json.Marshal(req.Tools)
	diagnostics.Event(ctx, "info", "chat.request.validated", "requested_model", req.Model, "stream", req.Stream, "message_count", len(req.Messages), "roles", roles, "client_tool_count", len(names), "client_tool_names", toolNames, "tool_schema_bytes", len(schemas), "tool_schema_sha256", diagnostics.Fingerprint(string(schemas)))
}

func logPreparedContext(ctx context.Context, in ChatInput, bridge bool, choiceMode, choiceName string) {
	combined := joinContext(in.SystemPrompt, in.ToolPrompt)
	diagnostics.Event(ctx, "info", "chat.context.prepared", "mapped_model", in.Model, "prompt_bytes", len(in.Prompt), "prompt_sha256", diagnostics.Fingerprint(in.Prompt), "custom_prompt_present", combined != "", "custom_prompt_bytes", len(combined), "custom_prompt_sha256", diagnostics.Fingerprint(combined), "application_instructions_bytes", len(in.SystemPrompt), "tool_protocol_bytes", len(in.ToolPrompt), "system_wire_field", "custom_prompt", "tool_bridge", bridge, "tool_choice_mode", choiceMode, "tool_choice_name", choiceName, "attachments", attachmentMetadata(in.Attachments), "thread_reused", in.ThreadID != "")
}

func attachmentMetadata(items []Attachment) []any {
	out := make([]any, 0, len(items))
	for _, a := range items {
		out = append(out, map[string]any{"type": a.Type, "mime_type": a.MIMEType, "size_bytes": a.Size, "sha256": a.ContentSHA256})
	}
	return out
}

func logClientCall(ctx context.Context, id, name, args string) {
	var obj map[string]any
	_ = json.Unmarshal([]byte(args), &obj)
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	diagnostics.Event(ctx, "info", "chat.client_tool.emitted", "tool_id", id, "tool_name", name, "arguments_bytes", len(args), "arguments_sha256", diagnostics.Fingerprint(args), "argument_keys", keys, "arguments_valid_json", json.Valid([]byte(args)))
}

// Only structural information is recorded, never arbitrary payload contents.
func nativeEventMetadata(payload json.RawMessage) map[string]any {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(payload, &obj)
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := map[string]any{"payload_bytes": len(payload), "payload_keys": keys}
	for _, key := range []string{"tool_name", "tool_id", "tool_call_id", "toolCallId", "error_code", "status", "continuation_expected", "is_error", "input_tokens", "output_tokens", "total_tokens", "cache_read_tokens", "cache_write_tokens", "context_window", "model", "resolved_model_id", "served_model_id"} {
		if raw, ok := obj[key]; ok {
			var value any
			if json.Unmarshal(raw, &value) == nil {
				switch value.(type) {
				case string, bool, float64:
					out[key] = value
				}
			}
		}
	}
	for _, key := range []string{"text", "delta", "arguments", "rawInput", "raw_input", "result", "input_delta"} {
		if raw, ok := obj[key]; ok {
			out[key+"_bytes"] = len(raw)
		}
	}
	return out
}
