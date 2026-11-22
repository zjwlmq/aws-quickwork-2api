package proxy

import "testing"

func TestTranscriptFingerprintsPreserveLargeIntegerArguments(t *testing.T) {
	base := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Function: FunctionCall{Name: "Read", Arguments: `{"record":9007199254740992}`}}}}
	other := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Function: FunctionCall{Name: "Read", Arguments: `{"record":9007199254740993}`}}}}
	for _, history := range [][]Message{{base, other}, canonicalToolHistory([]Message{base, other}, nil)} {
		if chatMessageFingerprint(history[0]) == chatMessageFingerprint(history[1]) {
			t.Fatal("distinct integer arguments collapsed")
		}
	}
	text := Message{Role: "assistant", Content: "reply"}
	blocks := Message{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "reply"}}}
	if chatMessageFingerprint(text) != chatMessageFingerprint(blocks) {
		t.Fatal("equivalent text encodings forked")
	}
}
