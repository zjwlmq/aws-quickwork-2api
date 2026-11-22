package proxy

import (
	"strings"
	"testing"
)

func TestSystemPromptPreservesClientRolesAndOrder(t *testing.T) {
	got := systemPrompt([]Message{
		{Role: "system", Content: "You are a coding agent."},
		{Role: "developer", Content: "Be terse."},
	})
	if !strings.Contains(got, "[SYSTEM INSTRUCTIONS]\nYou are a coding agent.\n\n[DEVELOPER INSTRUCTIONS]\nBe terse.") {
		t.Fatalf("client instruction roles/order were lost:\n%s", got)
	}
}

func TestSystemPromptWithoutClientInstructions(t *testing.T) {
	got := systemPrompt([]Message{{Role: "user", Content: "hi"}})
	if got != "" {
		t.Fatalf("ordinary chat must not receive invented application instructions: %s", got)
	}
}
