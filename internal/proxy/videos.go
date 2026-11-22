package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type videoRequest struct {
	Prompt         string          `json:"prompt"`
	Model          string          `json:"model"`
	Seconds        json.RawMessage `json:"seconds"`
	Size           string          `json:"size"`
	InputReference string          `json:"input_reference"`
	ImageURL       any             `json:"image_url"`
}

func (s *Server) videoAPI(w http.ResponseWriter, r *http.Request) {
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, 401, "invalid_api_key", "invalid proxy API key")
		return
	}
	owner := ownerKey(tokenFromRequest(r))
	if r.PathValue("id") == "" {
		switch r.Method {
		case http.MethodPost:
			s.CreateVideoJob(w, r, owner)
		case http.MethodGet:
			s.ListVideoJobs(w, r, owner)
		default:
			writeOpenAIError(w, 405, "method_not_allowed", "GET or POST required")
		}
	} else if r.Method == http.MethodGet {
		if strings.HasSuffix(r.URL.Path, "/content") {
			s.VideoJobContent(w, r, owner)
		} else {
			s.GetVideoJob(w, r, owner)
		}
	} else {
		writeOpenAIError(w, 405, "method_not_allowed", "GET required")
	}
}

// The empty owner is reserved for the authenticated administrator wrapper.
func (s *Server) ListVideoJobs(w http.ResponseWriter, r *http.Request, owner string) {
	if s.store == nil {
		writeOpenAIError(w, 503, "media_unavailable", "media storage is unavailable")
		return
	}
	jobs, err := s.store.ListVideos(r.Context(), owner)
	if err != nil {
		writeOpenAIError(w, 500, "storage_error", "读取视频任务失败")
		return
	}
	for i := range jobs {
		s.decorateVideo(r.Context(), &jobs[i])
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": jobs})
}

func (s *Server) decorateVideo(ctx context.Context, j *store.VideoJob) {
	j.Object = "video"
	if j.FileID != "" {
		if f, err := s.store.GetMedia(ctx, j.FileID); err == nil {
			j.URL = s.mediaURL(f)
			f.URL = j.URL
			j.Result = replaceNativeLinks(j.Result, []store.MediaFile{*f})
		}
	}
}

func (s *Server) ownedVideo(w http.ResponseWriter, r *http.Request, owner string) (*store.VideoJob, bool) {
	if s.store == nil {
		writeOpenAIError(w, 503, "media_unavailable", "media storage is unavailable")
		return nil, false
	}
	j, err := s.store.GetVideo(r.Context(), r.PathValue("id"))
	if err != nil || (owner != "" && j.Owner != owner) {
		writeOpenAIError(w, 404, "not_found", "视频任务不存在")
		return nil, false
	}
	s.decorateVideo(r.Context(), j)
	return j, true
}

func (s *Server) GetVideoJob(w http.ResponseWriter, r *http.Request, owner string) {
	if j, ok := s.ownedVideo(w, r, owner); ok {
		writeJSON(w, 200, j)
	}
}
func (s *Server) VideoJobContent(w http.ResponseWriter, r *http.Request, owner string) {
	j, ok := s.ownedVideo(w, r, owner)
	if !ok {
		return
	}
	if j.Status != "completed" || j.URL == "" {
		writeOpenAIError(w, 409, "video_not_ready", "视频尚未生成完成")
		return
	}
	http.Redirect(w, r, j.URL, http.StatusTemporaryRedirect)
}

func (s *Server) CreateVideoJob(w http.ResponseWriter, r *http.Request, owner string) {
	if s.store == nil {
		writeOpenAIError(w, 503, "media_unavailable", "media storage is unavailable")
		return
	}
	select {
	case s.videoSlots <- struct{}{}:
	default:
		writeOpenAIError(w, 429, "video_busy", "已有两个视频任务正在生成，请稍后重试")
		return
	}
	release := true
	defer func() {
		if release {
			<-s.videoSlots
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBody)
	var input videoRequest
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "视频请求表单无效或过大")
			return
		}
		defer r.MultipartForm.RemoveAll()
		input.Prompt = r.FormValue("prompt")
		input.Model = r.FormValue("model")
		input.Size = r.FormValue("size")
		input.Seconds, _ = json.Marshal(r.FormValue("seconds"))
		file, _, err := r.FormFile("input_reference")
		if err == nil {
			defer file.Close()
			data, err := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
			if err != nil || len(data) > maxImageBytes {
				writeOpenAIError(w, 400, "invalid_media", "参考图片不能超过 10 MiB")
				return
			}
			input.InputReference = "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
		} else if err != http.ErrMissingFile {
			writeOpenAIError(w, 400, "invalid_media", "参考图片读取失败")
			return
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "视频请求必须是 JSON 或 multipart/form-data")
			return
		}
	}
	input.Prompt = strings.TrimSpace(input.Prompt)
	if input.Prompt == "" || len(input.Prompt) > 20000 {
		writeOpenAIError(w, 400, "invalid_request_error", "prompt 必填，且不能超过 20000 字节")
		return
	}
	if input.Model == "" {
		input.Model = s.cfg.DefaultModel
	}
	valid := false
	for _, model := range s.cfg.Models {
		if input.Model == model {
			valid = true
		}
	}
	if !valid {
		writeOpenAIError(w, 400, "invalid_model", "请选择已配置的 Quick 模式，例如 smart")
		return
	}
	seconds := "5"
	if len(input.Seconds) > 0 {
		var value string
		if json.Unmarshal(input.Seconds, &value) == nil {
			if value != "" {
				seconds = value
			}
		} else {
			seconds = string(input.Seconds)
		}
	}
	n, err := strconv.Atoi(seconds)
	if err != nil || n < 1 || n > 30 {
		writeOpenAIError(w, 400, "invalid_request_error", "seconds 必须是 1 到 30 的整数")
		return
	}
	if input.Size == "" {
		input.Size = "1280x720"
	}
	switch input.Size {
	case "1280x720", "720x1280", "1024x1024", "1920x1080":
	default:
		writeOpenAIError(w, 400, "invalid_request_error", "size 支持 1280x720、720x1280、1024x1024、1920x1080")
		return
	}
	attachments := []Attachment{}
	if input.InputReference != "" || input.ImageURL != nil {
		image := input.ImageURL
		if input.InputReference != "" {
			image = input.InputReference
		}
		a, err := parseAttachment(r.Context(), map[string]any{"type": "image_url", "image_url": image}, 1)
		if err != nil {
			writeOpenAIError(w, 400, "invalid_media", err.Error())
			return
		}
		attachments = append(attachments, a)
	}
	encoded, _ := json.Marshal(attachments)
	j := store.VideoJob{ID: "video-" + strings.ReplaceAll(uuid(), "-", ""), Owner: owner, Prompt: input.Prompt, Model: input.Model, Status: "queued", Seconds: seconds, Size: input.Size, CreatedAt: time.Now().Unix(), InputJSON: string(encoded), Object: "video"}
	if err := s.store.CreateVideo(r.Context(), &j); err != nil {
		writeOpenAIError(w, 500, "storage_error", "视频任务保存失败")
		return
	}
	release = false
	diagnostics.Event(r.Context(), "info", "video.job.queued", "job_id", j.ID, "model", j.Model, "seconds", seconds, "size", j.Size, "prompt_bytes", len(j.Prompt), "prompt_sha256", diagnostics.Fingerprint(j.Prompt), "attachments", attachmentMetadata(attachments))
	go s.runVideo(context.WithoutCancel(r.Context()), j, attachments)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) runVideo(parent context.Context, j store.VideoJob, attachments []Attachment) {
	defer func() { <-s.videoSlots }()
	ctx, cancel := context.WithTimeout(diagnostics.With(parent, "job_id", j.ID), 30*time.Minute)
	defer cancel()
	started := time.Now()
	diagnostics.Event(ctx, "info", "video.job.started")
	j.Status = "in_progress"
	j.InputJSON = "[]"
	if err := s.store.UpdateVideo(ctx, &j); err != nil {
		diagnostics.Event(ctx, "error", "video.job.save_failed", "error", err)
	}
	finish := func(err error) {
		j.CompletedAt = time.Now().Unix()
		if err != nil {
			j.Status = "failed"
			j.Error = err.Error()
		} else {
			j.Status = "completed"
		}
		saveCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if saveErr := s.store.UpdateVideo(saveCtx, &j); saveErr != nil {
			diagnostics.Event(ctx, "error", "video.job.save_failed", "error", saveErr)
		}
		level := "info"
		if err != nil {
			level = "error"
		}
		diagnostics.Event(ctx, level, "video.job.finished", "status", j.Status, "duration_ms", time.Since(started).Milliseconds(), "file_id", j.FileID, "result_bytes", len(j.Result), "error", err)
	}
	prompt := fmt.Sprintf("请根据下面用户的描述，使用当前 Quick 会话中可用的视频制作工具，实际制作并交付一个 MP4 视频文件。视频长度 %s 秒，画面 %s；有参考图时应使用参考图。不要只返回脚本或制作步骤。请完成渲染后用 open_in_session_tab 展示文件，并在回复中提供 qw-file:// 文件链接。用户已经确认直接制作，不需要再询问方案。不要声称使用了当前账号未提供的模型或工具；如果无法制作，应明确说明原因。\n\n用户描述：\n%s", j.Seconds, j.Size, j.Prompt)
	in := ChatInput{Prompt: prompt, Model: j.Model, Title: titleFor(j.Prompt), Attachments: attachments, KeepSession: true}
	var res ChatResult
	var lastErr error
	for attempt := range s.maxAttempts(ctx) {
		attemptCtx := diagnostics.With(ctx, "attempt", attempt+1)
		diagnostics.Event(attemptCtx, "info", "video.attempt.started")
		acct, err := s.accounts.Next(attemptCtx)
		if err != nil {
			diagnostics.Event(attemptCtx, "error", "video.attempt.account_failed", "error", err)
			lastErr = err
			break
		}
		sess := SessionFromAccount(acct, s.cfg)
		in.ArtifactHandler = func(ctx context.Context, thread string, refs []NativeArtifact) ([]store.MediaFile, error) {
			return s.cacheArtifacts(ctx, sess, j.Owner, thread, refs)
		}
		res, err = s.upstream.Chat(attemptCtx, sess, in, nil)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		s.accounts.ReportFailure(attemptCtx, acct.ID, err)
		if ctx.Err() != nil {
			break
		}
	}
	j.Result = res.Text
	if lastErr != nil {
		finish(lastErr)
		return
	}
	for _, f := range res.Files {
		if strings.HasPrefix(f.MIMEType, "video/") {
			j.FileID = f.ID
			break
		}
	}
	if j.FileID == "" {
		finish(fmt.Errorf("Quick 未返回可下载的视频文件，请查看生成说明"))
		return
	}
	finish(nil)
}
