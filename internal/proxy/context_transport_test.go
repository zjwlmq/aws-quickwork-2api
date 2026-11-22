package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quickwork-openai/internal/config"
)

func readContextArchive(t *testing.T, in ChatInput) nativeContextArchive {
	t.Helper()
	attachment := in.Attachments[len(in.Attachments)-1]
	compressed, err := base64.StdEncoding.DecodeString(attachment.Data)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var archive nativeContextArchive
	if err := json.Unmarshal(body, &archive); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestLongContextPreservesInstructionsHistoryAndToolSchemas(t *testing.T) {
	protocol := buildToolBridgePrompt([]Tool{{Function: ToolFunction{Name: "Read", Description: strings.Repeat("Read client files only. 中文规范。", 1800), Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}}}, nil, "required", "", false)
	input := ChatInput{Prompt: "User: start\n" + strings.Repeat("Assistant tool call (id=read_1, name=Read): {\"path\":\"src/main.go\"}\nTool result (call_id=read_1, name=Read): 完整代码\n", 900) + "User: finish", SystemPrompt: "[SYSTEM INSTRUCTIONS]\nproject_nonce=PRIVATE_INSTRUCTIONS", ToolPrompt: protocol, Attachments: []Attachment{{Type: "file", Name: "original.txt", Data: "eA=="}}}
	packed, err := prepareNativeContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(packed.SystemPrompt, input.SystemPrompt) {
		t.Fatal("application instructions lost their native custom_prompt position")
	}
	if len(packed.Attachments) != 2 || packed.Attachments[0] != input.Attachments[0] {
		t.Fatal("existing attachments changed")
	}
	archive := readContextArchive(t, packed)
	if archive.Conversation != input.Prompt || archive.ClientToolProtocol != protocol {
		t.Fatal("history, tool-result correlation, descriptions or parameter schemas were altered")
	}
	if size := nativeContextFrameSize(packed); size > nativeFrameBudget {
		t.Fatalf("packed wire envelope still exceeds native budget: %d", size)
	}
	second, err := prepareNativeContext(context.Background(), packed)
	if err != nil || len(second.Attachments) != len(packed.Attachments) || second.SystemPrompt != packed.SystemPrompt {
		t.Fatal("prepared context was compressed twice")
	}
}

func TestLongApplicationInstructionsArePreservedInArchive(t *testing.T) {
	input := ChatInput{Prompt: "Read both project rules", SystemPrompt: "FIRST_RULE\n" + strings.Repeat("project instructions 规范\n", 10000) + "LAST_RULE"}
	packed, err := prepareNativeContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	archive := readContextArchive(t, packed)
	if archive.SystemInstructions != input.SystemPrompt || archive.Conversation != input.Prompt || !strings.Contains(packed.SystemPrompt, "system_instructions as application instructions") {
		t.Fatal("large application instructions were dropped or not assigned their instruction role")
	}
}

func TestOversizedIncompressibleContextReturns400BeforeAccountSelection(t *testing.T) {
	noise := make([]byte, 100<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	server := NewServer(&cfg, nil)
	body, _ := json.Marshal(map[string]any{"model": "smart", "stream": true, "messages": []any{map[string]any{"role": "user", "content": base64.StdEncoding.EncodeToString(noise)}}})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	response := httptest.NewRecorder()
	// A nil account manager deliberately catches any attempt to select or penalize
	// an account for client input that cannot fit the native transport.
	server.handleChat(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "context_length_exceeded") || strings.Contains(response.Header().Get("Content-Type"), "event-stream") {
		t.Fatalf("expected an input error before streaming: %d %s", response.Code, response.Body.String())
	}
}

func TestSmallContextKeepsOriginalInlineBehavior(t *testing.T) {
	packed, err := prepareNativeContext(context.Background(), ChatInput{Prompt: "user question", SystemPrompt: "project rules", ToolPrompt: "tool contract"})
	if err != nil || packed.Prompt != "user question" || packed.SystemPrompt != "project rules\n\ntool contract" || len(packed.Attachments) != 0 {
		t.Fatalf("ordinary context behavior changed: %#v %v", packed, err)
	}
}
