package proxy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func readContextArchive(t *testing.T, in ChatInput) nativeContextArchive {
	t.Helper()
	body := in.Uploads[len(in.Uploads)-1].Bytes
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
	if len(packed.Attachments) != 2 || len(packed.Uploads) != 2 || string(packed.Uploads[0].Bytes) != "x" || packed.Attachments[0].Name != "original.txt" {
		t.Fatal("existing attachment bytes or metadata changed")
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
		t.Fatal("prepared context was packed twice")
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

func TestOversizedIncompressibleContextUsesCompleteUncompressedUpload(t *testing.T) {
	noise := make([]byte, 100<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	input := ChatInput{Prompt: base64.StdEncoding.EncodeToString(noise)}
	packed, err := prepareNativeContext(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if readContextArchive(t, packed).Conversation != input.Prompt || len(packed.Uploads[0].Bytes) < len(input.Prompt) || packed.Attachments[0].MIMEType != "application/json" || packed.Attachments[0].Data != "" {
		t.Fatal("input was compressed, changed or left in WS payload")
	}
}

func TestNativeLimitExcludesBase64Envelope(t *testing.T) {
	input := ChatInput{Prompt: strings.Repeat("x", 61<<10)}
	packed, err := prepareNativeContext(context.Background(), input)
	if err != nil || len(packed.Uploads) != 0 || packed.Prompt != input.Prompt {
		t.Fatalf("native input fitting 64 KiB was unnecessarily offloaded: %v", err)
	}
}

func TestSmallContextKeepsOriginalInlineBehavior(t *testing.T) {
	packed, err := prepareNativeContext(context.Background(), ChatInput{Prompt: "user question", SystemPrompt: "project rules", ToolPrompt: "tool contract"})
	if err != nil || packed.Prompt != "user question" || packed.SystemPrompt != "project rules\n\ntool contract" || len(packed.Attachments) != 0 {
		t.Fatalf("ordinary context behavior changed: %#v %v", packed, err)
	}
}
