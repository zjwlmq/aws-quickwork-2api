package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"time"

	"quickwork-openai/internal/diagnostics"
)

// Quick can return MPEG-4 Part 2 inside MP4. Check the real stream and
// normalize to H.264/yuv420p for browser playback. Inputs are local cached
// files, and the decoder cannot fetch additional network resources.
func browserMP4(ctx context.Context, filename string) (int64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	output, err := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe", "-select_streams", "v:0", "-show_entries", "stream=codec_name,pix_fmt", "-of", "json", filename).Output()
	cancel()
	if err != nil {
		logEncoderError(ctx, "video.probe.failed", err)
		return 0, errors.New("视频无法解码；请确认服务器已安装 ffmpeg 和 ffprobe")
	}
	var probe struct {
		Streams []struct {
			Codec string `json:"codec_name"`
			Pixel string `json:"pix_fmt"`
		} `json:"streams"`
	}
	if json.Unmarshal(output, &probe) != nil || len(probe.Streams) == 0 {
		return 0, errors.New("Quick 返回的文件中没有可用的视频流")
	}
	if probe.Streams[0].Codec != "h264" || probe.Streams[0].Pixel != "yuv420p" {
		diagnostics.Event(ctx, "info", "video.transcode.started", "source_codec", probe.Streams[0].Codec, "source_pixel_format", probe.Streams[0].Pixel, "target_codec", "h264", "target_pixel_format", "yuv420p")
		started := time.Now()
		converted := filename + ".converted"
		defer os.Remove(converted)
		conversionCtx, done := context.WithTimeout(ctx, 3*time.Minute)
		_, err := exec.CommandContext(conversionCtx, "ffmpeg", "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe", "-i", filename, "-map", "0:v:0", "-map", "0:a?", "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", "-f", "mp4", "-y", converted).Output()
		done()
		if err != nil {
			logEncoderError(ctx, "video.transcode.failed", err)
			return 0, errors.New("视频兼容转码失败，请重试或缩短视频时长")
		}
		if err = os.Chmod(converted, 0600); err != nil {
			return 0, err
		}
		info, err := os.Stat(converted)
		if err != nil || info.Size() > 256<<20 {
			return 0, errors.New("转码后的视频超过 256 MiB 限制")
		}
		if err = os.Remove(filename); err != nil {
			return 0, err
		}
		if err = os.Rename(converted, filename); err != nil {
			return 0, err
		}
		diagnostics.Event(ctx, "info", "video.transcode.completed", "duration_ms", time.Since(started).Milliseconds(), "size_bytes", info.Size())
	}
	diagnostics.Event(ctx, "debug", "video.probe.completed", "source_codec", probe.Streams[0].Codec, "source_pixel_format", probe.Streams[0].Pixel)
	info, err := os.Stat(filename)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func logEncoderError(ctx context.Context, event string, err error) {
	var exit *exec.ExitError
	detail := ""
	exitCode := -1
	if errors.As(err, &exit) {
		detail = string(exit.Stderr)
		exitCode = exit.ExitCode()
	}
	diagnostics.Event(ctx, "error", event, "error", err, "exit_code", exitCode, "stderr", detail, "context_error", ctx.Err())
}
