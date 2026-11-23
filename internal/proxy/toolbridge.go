package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolCallReply is a client-side tool call parsed from the model's text.
type ToolCallReply struct {
	ID        string
	Name      string
	Arguments string
}

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type toolCallEnvelope struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

const (
	clientToolCallOpen  = "<client_tool_call>"
	clientToolCallClose = "</client_tool_call>"
)

func collectToolDefinitions(tools []Tool, functions []ToolFunction) []toolDefinition {
	defs := make([]toolDefinition, 0, len(tools)+len(functions))
	for _, t := range tools {
		f := t.Function
		if strings.TrimSpace(f.Name) == "" {
			continue
		}
		defs = append(defs, toolDefinition{Name: f.Name, Description: f.Description, Parameters: f.Parameters})
	}
	for _, f := range functions {
		if strings.TrimSpace(f.Name) == "" {
			continue
		}
		defs = append(defs, toolDefinition{Name: f.Name, Description: f.Description, Parameters: f.Parameters})
	}
	return defs
}

// buildToolBridgePrompt teaches the upstream model a small text protocol for
// tools that only the caller can execute. QuickWork does not accept arbitrary
// client tool schemas, so the proxy carries an RPC contract in custom_prompt.
func buildToolBridgePrompt(tools []Tool, functions []ToolFunction, mode, name string, parallel bool) string {
	if mode == "none" {
		return ""
	}
	defs := collectToolDefinitions(tools, functions)
	if len(defs) == 0 {
		return ""
	}
	body, err := json.Marshal(defs)
	if err != nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("<client_tool_protocol>\n")
	b.WriteString("Transport encoding for the client's tool channel: keep the application/project instructions above unchanged. When those instructions refer to native tool calls, serialize those calls using the marker below; the proxy converts them to native client tool events. This changes the wire encoding, not the requested behavior or tool permissions.\n")
	b.WriteString("You are composing a response for a separate client application. The client exposes the following RPC operations on its own machine:\n")
	b.Write(body)
	b.WriteString("\n\nThese operations run in the external client, not in the Quick workspace. You do not need direct access to that machine, its folders, or an outpost. The client application reads the serialized request below, executes it with its own permissions, and sends back the result. To fulfill tasks involving the client's files, compose an RPC request using the listed schema. Do not claim a tool succeeded until a matching tool result arrives. This is an output serialization protocol, not a request to execute an unavailable native Quick tool.\n")
	b.WriteString("To call a tool, output exactly one marker per call and no other text in that message:\n")
	b.WriteString(`<client_tool_call>{"name":"TOOL_NAME","arguments":{...}}</client_tool_call>`)
	b.WriteString("\nThe caller executes the tool and returns a tool result. After receiving it, continue the task. Do not wrap a tool call in Markdown fences. Do not add prose around a tool call.\n")
	b.WriteString("For client project/file operations, use these client RPC operations. Native Quick Python/filesystem tools can read transported context attachments, but their workspace is separate from the client's project. Do not create the client's deliverable in that workspace as a substitute for a client Write/Patch call. Put the complete requested content in the client tool arguments; a native Python variable or a placeholder such as __SVG__ cannot transfer that content to the client. Follow an explicit request for literal placeholder text when that is the actual requested file content. The marker must contain a JSON object, not a JSON-escaped representation of one.\n")
	if mode == "function" && name != "" {
		fmt.Fprintf(&b, "You must call the tool %q.\n", name)
	}
	if mode == "required" {
		b.WriteString("You must call at least one tool.\n")
	}
	if !parallel {
		b.WriteString("Call at most one tool per turn.\n")
	}
	b.WriteString("</client_tool_protocol>")
	return b.String()
}

func parseToolEnvelopes(payload string) []toolCallEnvelope {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	// Some native replies quote the complete JSON object or escape it once
	// inside the marker. Only unwrap a complete, valid JSON value; never try
	// to complete truncated tool arguments.
	var decoded string
	if json.Unmarshal([]byte(payload), &decoded) == nil && json.Valid([]byte(decoded)) {
		payload = decoded
	} else if !json.Valid([]byte(payload)) && json.Unmarshal([]byte(`"`+payload+`"`), &decoded) == nil && json.Valid([]byte(decoded)) {
		payload = decoded
	}
	var arr []toolCallEnvelope
	if strings.HasPrefix(payload, "[") && json.Unmarshal([]byte(payload), &arr) == nil {
		return arr
	}
	var wrapper struct {
		ToolCalls []toolCallEnvelope `json:"tool_calls"`
	}
	if json.Unmarshal([]byte(payload), &wrapper) == nil && len(wrapper.ToolCalls) > 0 {
		return wrapper.ToolCalls
	}
	var one toolCallEnvelope
	if json.Unmarshal([]byte(payload), &one) == nil {
		return []toolCallEnvelope{one}
	}
	return nil
}

func repliesFromEnvelopes(envelopes []toolCallEnvelope, allowed map[string]bool) []ToolCallReply {
	out := make([]ToolCallReply, 0, len(envelopes))
	for _, env := range envelopes {
		name := strings.TrimSpace(env.Name)
		if name == "" || !allowed[name] {
			continue
		}
		args := argumentString(env.Arguments)
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, ToolCallReply{
			ID:        "call_" + strings.ReplaceAll(uuid(), "-", ""),
			Name:      name,
			Arguments: args,
		})
	}
	return out
}

// extractToolCalls removes valid client tool-call markers from text and
// returns the remaining assistant text. Unknown tool names are left untouched.
func extractToolCalls(text string, allowed map[string]bool) (string, []ToolCallReply) {
	if len(allowed) == 0 {
		return text, nil
	}
	var clean strings.Builder
	var replies []ToolCallReply
	rest := text
	for {
		start := strings.Index(rest, clientToolCallOpen)
		if start < 0 {
			clean.WriteString(rest)
			break
		}
		end := strings.Index(rest[start+len(clientToolCallOpen):], clientToolCallClose)
		if end < 0 {
			clean.WriteString(rest)
			break
		}
		end += start + len(clientToolCallOpen)
		clean.WriteString(rest[:start])
		payload := rest[start+len(clientToolCallOpen) : end]
		replies = append(replies, repliesFromEnvelopes(parseToolEnvelopes(payload), allowed)...)
		rest = rest[end+len(clientToolCallClose):]
	}
	if len(replies) == 0 {
		return text, nil
	}
	return strings.TrimSpace(clean.String()), replies
}

// extractBareToolCall handles models that answer with a single JSON object
// instead of the requested marker.
func extractBareToolCall(text string, allowed map[string]bool) (string, []ToolCallReply) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text, nil
	}
	if strings.HasPrefix(trimmed, "```") {
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			trimmed = strings.TrimSuffix(strings.TrimSpace(trimmed[i+1:]), "```")
			trimmed = strings.TrimSpace(trimmed)
		}
	}
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return text, nil
	}
	replies := repliesFromEnvelopes(parseToolEnvelopes(trimmed), allowed)
	if len(replies) == 0 {
		return text, nil
	}
	return "", replies
}

func parseClientToolCalls(text string, allowed map[string]bool) (string, []ToolCallReply) {
	clean, replies := extractToolCalls(text, allowed)
	if len(replies) > 0 {
		return clean, replies
	}
	return extractBareToolCall(text, allowed)
}

func replyToolCalls(replies []ToolCallReply) []ToolCall {
	out := make([]ToolCall, 0, len(replies))
	for _, r := range replies {
		out = append(out, ToolCall{
			ID:   r.ID,
			Type: "function",
			Function: FunctionCall{
				Name:      r.Name,
				Arguments: r.Arguments,
			},
		})
	}
	return out
}
