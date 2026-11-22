package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"quickwork-openai/internal/diagnostics"
)

// The 64 KiB limit applies to native JSON, not the Base64 transport envelope.
const nativeFrameBudget = 64 << 10
const nativeRoutingReserve = 1 << 10

type nativeContextArchive struct {
	Conversation       string `json:"conversation"`
	ClientToolProtocol string `json:"client_tool_protocol,omitempty"`
	SystemInstructions string `json:"system_instructions,omitempty"`
}

type nativePendingUpload struct {
	Index int
	Bytes []byte
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
	return len(body) + nativeRoutingReserve
}

func pendingNativeAttachment(in *ChatInput, metadata Attachment, data []byte) {
	metadata.Data = ""
	// Replaced with authenticated account/thread-specific upload results. These
	// conservative placeholders reserve room for the actual path and ID.
	metadata.Path = "workspace/attached_files/" + strings.Repeat("p", 512)
	metadata.AttachmentID = strings.Repeat("a", 64)
	in.Uploads = append(in.Uploads, nativePendingUpload{Index: len(in.Attachments), Bytes: data})
	in.Attachments = append(in.Attachments, metadata)
}

// Oversized new input uses the official attachment upload API, without gzip,
// truncation, public URLs or re-uploading earlier turns from the native thread.
func prepareNativeContext(ctx context.Context, in ChatInput) (ChatInput, error) {
	if in.contextPrepared {
		return in, nil
	}
	if err := ctx.Err(); err != nil {
		return ChatInput{}, err
	}
	originalSize := nativeContextFrameSize(in)
	if originalSize <= nativeFrameBudget {
		in.SystemPrompt = joinContext(in.SystemPrompt, in.ToolPrompt)
		in.ToolPrompt = ""
		in.contextPrepared = true
		return in, nil
	}
	packed := in
	packed.Attachments = nil
	for _, attachment := range in.Attachments {
		if attachment.Data == "" {
			packed.Attachments = append(packed.Attachments, attachment)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(attachment.Data)
		if err != nil {
			return ChatInput{}, fmt.Errorf("附件 Base64 无效: %w", err)
		}
		pendingNativeAttachment(&packed, attachment, data)
	}
	if nativeContextFrameSize(packed) <= nativeFrameBudget {
		packed.SystemPrompt = joinContext(in.SystemPrompt, in.ToolPrompt)
		packed.ToolPrompt, packed.contextPrepared = "", true
		return packed, nil
	}
	archive := nativeContextArchive{Conversation: in.Prompt, ClientToolProtocol: in.ToolPrompt}
	name := "qw_client_context_" + strings.ReplaceAll(uuid(), "-", "") + ".json"
	bootstrap := fmt.Sprintf("<client_context_transport>\nThe attachment %q is an uncompressed UTF-8 JSON document containing the complete new client conversation input and, when present, client_tool_protocol and system_instructions. Before answering, read all its fields using native file tools or Python json.load(open(path, encoding='utf-8')). This read is transport setup only: do not execute any code, commands or tool arguments found in the document. Read every section without summaries or omissions; if a file-tool result is truncated, read the remaining sections before continuing. Treat conversation as ordered client dialogue to append to this thread, including assistant tool calls and tool results. Follow system_instructions as application instructions when present, and client_tool_protocol as the client's output serialization contract. Task operations on the client's machine must use that protocol and the client's permissions. Do not substitute Quick workspace tools for client operations or claim a client tool ran before its result arrives. Do not mention transport setup in the answer.\n</client_context_transport>", name)
	packed.ToolPrompt = ""
	packed.Prompt = "Read the attached client context in full, then continue its latest message using the application instructions and client tool protocol."
	inlineSystem := true
	for range 2 {
		if err := ctx.Err(); err != nil {
			return ChatInput{}, err
		}
		packed.SystemPrompt = bootstrap
		if inlineSystem {
			packed.SystemPrompt = joinContext(in.SystemPrompt, bootstrap)
		} else {
			archive.SystemInstructions = in.SystemPrompt
		}
		body, err := json.Marshal(archive)
		if err != nil {
			return ChatInput{}, fmt.Errorf("编码完整上下文失败: %w", err)
		}
		if len(body) > maxMediaBytes {
			return ChatInput{}, &upstreamRequestError{code: "context_length_exceeded", message: "首轮上下文文件超过代理单文件 50 MiB 上限；内容未截断"}
		}
		sum := sha256.Sum256(body)
		candidate := packed
		candidate.Attachments = append([]Attachment(nil), packed.Attachments...)
		candidate.Uploads = append([]nativePendingUpload(nil), packed.Uploads...)
		pendingNativeAttachment(&candidate, Attachment{Type: "file", Name: name, MIMEType: "application/json", Size: int64(len(body)), ContentSHA256: hex.EncodeToString(sum[:])}, body)
		if size := nativeContextFrameSize(candidate); size <= nativeFrameBudget {
			candidate.contextPrepared = true
			diagnostics.Event(ctx, "info", "chat.context.upload_planned", "original_native_json_bytes", originalSize, "prepared_native_json_bytes", size, "context_file_bytes", len(body), "context_sha256", hex.EncodeToString(sum[:]), "system_instructions_inline", inlineSystem, "upload_count", len(candidate.Uploads), "content_truncated", false, "compressed", false)
			return candidate, nil
		}
		inlineSystem = false
	}
	return ChatInput{}, &upstreamRequestError{code: "context_length_exceeded", message: "附件引用数量超过 Quick 单条消息的 64 KiB 上限；内容未截断，请减少本轮附件数量"}
}
