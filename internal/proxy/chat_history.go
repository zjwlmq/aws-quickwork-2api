package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// Canonical hashes make the OpenAI string and Anthropic text-block encodings
// equivalent without storing the original transcript. Media is fingerprinted
// separately so changing an image cannot accidentally reuse another history.
func chatMessageFingerprint(m Message) string {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}
	var media []any
	if parts, ok := m.Content.([]any); ok {
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok || (p["type"] != "text" && p["type"] != "input_text") {
				media = append(media, part)
			}
		}
	}
	type call struct{ ID, Name, Arguments string }
	var calls []call
	for _, c := range m.ToolCalls {
		calls = append(calls, call{c.ID, c.Function.Name, canonicalArguments(c.Function.Arguments)})
	}
	var legacy *call
	if m.FunctionCall != nil {
		legacy = &call{Name: m.FunctionCall.Name, Arguments: canonicalArguments(m.FunctionCall.Arguments)}
	}
	body, _ := json.Marshal(struct {
		Role, Text, Name, ToolResultID string
		Media                          []any
		Calls                          []call
		Legacy                         *call
	}{role, contentText(m.Content), m.Name, m.ToolCallID, media, calls, legacy})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func canonicalArguments(s string) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil && json.Valid([]byte(s)) {
		b, _ := json.Marshal(value)
		return string(b)
	}
	return strings.TrimSpace(s)
}

func conversationMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		if !strings.EqualFold(m.Role, "system") && !strings.EqualFold(m.Role, "developer") {
			out = append(out, m)
		}
	}
	return out
}

func transcriptPrefixes(seed string, messages []Message) []string {
	out := make([]string, 0, len(messages))
	previous := seed
	for _, m := range messages {
		sum := sha256.Sum256([]byte(previous + ":" + chatMessageFingerprint(m)))
		previous = hex.EncodeToString(sum[:])
		out = append(out, previous)
	}
	return out
}

func withToolResultNames(delta, history []Message) []Message {
	names := make(map[string]string)
	for _, m := range history {
		for _, c := range m.ToolCalls {
			names[c.ID] = c.Function.Name
		}
	}
	out := append([]Message(nil), delta...)
	for i := range out {
		if out[i].Role == "tool" && out[i].Name == "" {
			out[i].Name = names[out[i].ToolCallID]
		}
	}
	return out
}

func chatToolSchemas(req *ChatRequest) map[string]map[string]any {
	out := make(map[string]map[string]any)
	for _, tool := range req.Tools {
		out[tool.Function.Name], _ = tool.Function.Parameters.(map[string]any)
	}
	for _, function := range req.Functions {
		out[function.Name], _ = function.Parameters.(map[string]any)
	}
	return out
}

// Clients may materialize JSON Schema defaults before replaying assistant tool
// calls. Only declared defaults are equivalent; arbitrary argument edits fork.
func canonicalToolHistory(history []Message, schemas map[string]map[string]any) []Message {
	out := append([]Message(nil), history...)
	for i := range out {
		out[i].ToolCalls = append([]ToolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			call := &out[i].ToolCalls[j]
			var value any
			decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
			decoder.UseNumber()
			if decoder.Decode(&value) == nil && json.Valid([]byte(call.Function.Arguments)) {
				value = applySchemaDefaults(value, schemas[call.Function.Name])
				body, _ := json.Marshal(value)
				call.Function.Arguments = string(body)
			}
		}
	}
	return out
}

func applySchemaDefaults(value any, schema map[string]any) any {
	switch object := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		for key, raw := range properties {
			property, _ := raw.(map[string]any)
			child, exists := object[key]
			if !exists {
				if fallback, ok := property["default"]; ok {
					// Clone defaults so recursive edits cannot mutate shared schemas.
					body, _ := json.Marshal(fallback)
					_ = json.Unmarshal(body, &child)
					object[key], exists = child, true
				}
			}
			if exists {
				object[key] = applySchemaDefaults(child, property)
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		for i := range object {
			object[i] = applySchemaDefaults(object[i], items)
		}
	}
	return value
}
