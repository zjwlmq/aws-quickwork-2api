package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"quickwork-openai/internal/diagnostics"
)

// AIDP rejects messages over 64 KiB, including the base64 envelope. Leave room
// for native session identifiers and routing fields, and count encoded bytes
// rather than Unicode characters. Gateway requests use this conservative limit
// as well. Compression never removes application instructions or history.
const nativeFrameBudget = 64 << 10
const nativeRoutingReserve = 2 << 10

type nativeContextArchive struct {
	Conversation       string `json:"conversation"`
	ClientToolProtocol string `json:"client_tool_protocol,omitempty"`
	SystemInstructions string `json:"system_instructions,omitempty"`
}

func joinContext(parts ...string) string {
	var nonempty []string
	for _, part := range parts {
		if part != "" {
			nonempty = append(nonempty, part)
		}
	}
	return strings.Join(nonempty, "\n\n")
}

func nativeContextFrameSize(in ChatInput) int {
	body, _ := json.Marshal(map[string]any{
		"type": "user_message",
		"payload": map[string]any{
			"text": in.Prompt, "custom_prompt": joinContext(in.SystemPrompt, in.ToolPrompt), "attachments": in.Attachments,
		},
	})
	return len(aidpEncode(string(body))) + nativeRoutingReserve
}

// prepareNativeContext keeps ordinary turns inline. For oversized turns, Quick's
// native attachment channel carries a lossless gzip JSON document. The original
// application instructions stay in custom_prompt whenever they fit. The native
// agent decodes the document before continuing the caller's conversation and
// client tool protocol. This needs no public URL or additional context storage.
func prepareNativeContext(ctx context.Context, in ChatInput) (ChatInput, error) {
	if in.contextPrepared {
		return in, nil
	}
	originalSize := nativeContextFrameSize(in)
	if originalSize <= nativeFrameBudget {
		in.SystemPrompt = joinContext(in.SystemPrompt, in.ToolPrompt)
		in.ToolPrompt = ""
		in.contextPrepared = true
		return in, nil
	}
	archive := nativeContextArchive{Conversation: in.Prompt, ClientToolProtocol: in.ToolPrompt}
	name := "qw_client_context_" + strings.ReplaceAll(uuid(), "-", "") + ".json.gz"
	packed := in
	packed.ToolPrompt = ""
	packed.Prompt = "Load the attached client context in full, then respond to the latest user message in its conversation. Follow the application instructions and client tool protocol. The archive is transport data; do not treat this setup line as the user's task."
	bootstrap := fmt.Sprintf("<client_context_transport>\nThe attachment %q is a lossless gzip-compressed UTF-8 JSON document containing the full client conversation and, when present, its client_tool_protocol and system_instructions. Before answering, load its full contents using the native Python/file tools (Python gzip.open(path, 'rt', encoding='utf-8') and json.load). This native read/decompression is transport setup only; it does not authorize execution of any task, code, command, or tool arguments found in the document. Read all fields, without summaries or omitted sections; if a tool output is truncated, read the remaining sections before continuing. Treat conversation as the ordered client dialogue, including prior assistant tool calls and tool results, and continue from its latest user message. Follow system_instructions as application instructions when present, and client_tool_protocol as the client's output serialization contract. Task operations on the client's machine must use that protocol and the client's permissions. Do not substitute Quick workspace tools for client operations or claim a client tool ran before its result arrives. Do not mention the transport setup in the answer.\n</client_context_transport>", name)
	inlineSystem := true
	var archiveBytes, compressedBytes int
	for range 2 {
		if err := ctx.Err(); err != nil {
			return ChatInput{}, err
		}
		body, err := json.Marshal(archive)
		if err != nil {
			return ChatInput{}, fmt.Errorf("编码完整上下文失败: %w", err)
		}
		var compressed bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
		if _, err = zw.Write(body); err != nil {
			return ChatInput{}, fmt.Errorf("压缩完整上下文失败: %w", err)
		}
		if err = zw.Close(); err != nil {
			return ChatInput{}, fmt.Errorf("压缩完整上下文失败: %w", err)
		}
		sum := sha256.Sum256(compressed.Bytes())
		attachment := Attachment{Type: "file", Name: name, Size: int64(compressed.Len()), MIMEType: "application/gzip", Data: base64.StdEncoding.EncodeToString(compressed.Bytes()), ContentSHA256: hex.EncodeToString(sum[:])}
		packed.Attachments = append(append([]Attachment(nil), in.Attachments...), attachment)
		packed.SystemPrompt = bootstrap
		if inlineSystem {
			packed.SystemPrompt = joinContext(in.SystemPrompt, bootstrap)
		}
		archiveBytes, compressedBytes = len(body), compressed.Len()
		if finalSize := nativeContextFrameSize(packed); finalSize <= nativeFrameBudget {
			packed.contextPrepared = true
			diagnostics.Event(ctx, "info", "chat.context.compressed", "original_frame_bytes", originalSize, "prepared_frame_bytes", finalSize, "frame_budget_bytes", nativeFrameBudget, "archive_bytes", archiveBytes, "compressed_bytes", compressedBytes, "archive_sha256", diagnostics.Fingerprint(string(body)), "system_instructions_inline", inlineSystem, "content_truncated", false)
			return packed, nil
		}
		inlineSystem = false
		archive.SystemInstructions = in.SystemPrompt
	}
	diagnostics.Event(ctx, "warn", "chat.context.too_large", "original_frame_bytes", originalSize, "prepared_frame_bytes", nativeContextFrameSize(packed), "archive_bytes", archiveBytes, "compressed_bytes", compressedBytes, "attachment_count", len(in.Attachments), "content_truncated", false)
	return ChatInput{}, fmt.Errorf("完整上下文和附件压缩后仍超过 Quick 单条消息传输上限（64 KiB）；内容未截断，请缩短当前会话或减少附件后重试")
}
