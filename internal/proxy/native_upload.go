package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
)

func nativeTenantURL(sess *Session, apiPath string, query url.Values) (string, error) {
	host := sess.TenantURL
	if host == "" {
		host = sess.WSHost
	}
	host = strings.Replace(strings.Replace(host, "wss://", "https://", 1), "ws://", "http://", 1)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	endpoint, err := url.Parse(host)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || (endpoint.Path != "" && endpoint.Path != "/") {
		return "", fmt.Errorf("invalid native upload host")
	}
	endpoint.Path, endpoint.RawQuery, endpoint.Fragment = apiPath, query.Encode(), ""
	return endpoint.String(), nil
}

func nativeUploadRequest(ctx context.Context, sess *Session, method, apiPath string, query url.Values, body []byte) (map[string]any, error) {
	endpoint, err := nativeTenantURL(sess, apiPath, query)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+sess.jwt())
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("x-amzn-qs-internal-trace-id", uuid())
	res, err := nativeHTTP.Do(r)
	if err != nil {
		return nil, fmt.Errorf("native upload request failed: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("native upload response unreadable or over 1 MiB")
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("native attachment HTTP %d: %s", res.StatusCode, errorText(json.RawMessage(raw), "", ""))
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil, fmt.Errorf("native upload returned invalid JSON")
	}
	return value, nil
}

func (u *Upstream) uploadAttachment(ctx context.Context, sess *Session, thread string, a Attachment, data []byte) (Attachment, error) {
	if isGatewayProvider(sess.Provider) {
		return Attachment{}, &upstreamRequestError{code: "unsupported_feature", message: "当前 Gateway 登录路径尚未适配大附件上传；请使用已支持的 social/enterprise 登录路径"}
	}
	if len(data) == 0 || len(data) > maxMediaBytes {
		return Attachment{}, &upstreamRequestError{code: "invalid_media", message: "上传附件必须为 1 字节至 50 MiB"}
	}
	value, err := nativeUploadRequest(ctx, sess, http.MethodPost, "/integration/quick-work/upload-attachment", url.Values{"name": {a.Name}, "contentType": {a.MIMEType}, "contentLength": {fmt.Sprint(len(data))}}, data)
	if err != nil {
		return Attachment{}, err
	}
	id, _ := value["attachmentId"].(string)
	if id == "" || len(id) > 256 {
		return Attachment{}, fmt.Errorf("native upload returned no valid attachment ID")
	}
	diagnostics.Event(ctx, "info", "upstream.attachment.uploaded", "bytes", len(data), "sha256", a.ContentSHA256, "attachment_id_hash", diagnostics.Fingerprint(id))
	allowed := false
	for range 60 {
		verdict, err := nativeUploadRequest(ctx, sess, http.MethodGet, "/integration/quick-work/attachment-dlp-status", url.Values{"attachmentId": {id}}, nil)
		if err != nil {
			return Attachment{}, err
		}
		status := strings.ToUpper(stringValue(verdict, "dlpJobStatus"))
		action := strings.ToUpper(stringValue(verdict, "policyAction"))
		if action == "BLOCK" || action == "WARN" || status == "FAILED" || status == "ERROR" {
			return Attachment{}, &upstreamRequestError{code: "input_file_rejected", message: "Quick 附件检查未放行该文件（" + status + "/" + action + "）"}
		}
		if status == "COMPLETED" || status == "SKIPPED" || status == "NOT_APPLICABLE" {
			allowed = action == "ALLOW"
			break
		}
		timer := time.NewTimer(1500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Attachment{}, ctx.Err()
		case <-timer.C:
		}
	}
	if !allowed {
		return Attachment{}, fmt.Errorf("native attachment inspection did not return ALLOW")
	}
	delivery, err := u.nativeAPI(ctx, sess, http.MethodPost, "files/upload/"+thread, map[string]any{"attachmentId": id})
	if err != nil {
		return Attachment{}, err
	}
	file, ok := delivery.(map[string]any)
	if !ok {
		return Attachment{}, fmt.Errorf("native attachment delivery returned invalid metadata")
	}
	relative, err := cleanNativePath(stringValue(file, "workspace_path"))
	if err != nil {
		return Attachment{}, err
	}
	filePath := stringValue(file, "path")
	if filePath == "" {
		filePath = relative
	}
	if strings.ContainsAny(filePath, "\x00\r\n") || (filePath != relative && (!strings.Contains(filePath, "/sessions/"+thread+"/") || !strings.HasSuffix(filePath, "/"+relative))) {
		return Attachment{}, fmt.Errorf("native attachment path does not belong to bound thread")
	}
	a.Path, a.AttachmentID, a.Data = filePath, id, ""
	diagnostics.Event(ctx, "info", "upstream.attachment.delivered", "workspace_path", relative, "bytes", len(data))
	return a, nil
}
