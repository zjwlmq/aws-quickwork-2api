package diagnostics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type Filter struct {
	RequestID string
	Level     string
	Search    string
	Limit     int
}
type Result struct {
	Entries      []map[string]any `json:"entries"`
	Truncated    bool             `json:"truncated"`
	ScannedBytes int64            `json:"scanned_bytes"`
	Level        string           `json:"configured_level"`
	File         string           `json:"file"`
	MaxMB        int64            `json:"max_mb"`
	Backups      int              `json:"backups"`
	WriteError   string           `json:"write_error,omitempty"`
}

// Query scans backwards, preserving a bounded cost even for large log files.
// Open descriptors under the rotation lock so renames cannot duplicate pages.
func (l *Logger) Query(filter Filter) (Result, error) {
	out := Result{Entries: []map[string]any{}, Level: l.levelName, File: l.path, MaxMB: l.maxBytes >> 20, Backups: l.backups}
	if filter.Limit < 1 {
		filter.Limit = 300
	}
	if filter.Limit > 5000 {
		filter.Limit = 5000
	}
	var files []*os.File
	l.mu.Lock()
	out.WriteError = l.lastWriteError
	var openErr error
	for i := 0; i <= l.backups; i++ {
		p := l.path
		if i > 0 {
			p = fmt.Sprintf("%s.%d", p, i)
		}
		if p == "" {
			break
		}
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			openErr = err
			break
		}
		files = append(files, f)
	}
	l.mu.Unlock()
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	if openErr != nil {
		return out, openErr
	}
	const scanLimit int64 = 32 << 20
	search := strings.ToLower(filter.Search)
	consume := func(line []byte) bool {
		if len(line) == 0 {
			return false
		}
		var entry map[string]any
		if json.Unmarshal(line, &entry) != nil {
			return false
		}
		if filter.RequestID != "" && entry["request_id"] != filter.RequestID {
			return false
		}
		level, _ := entry["level"].(string)
		if filter.Level != "" && levelNumber(level) < levelNumber(filter.Level) {
			return false
		}
		if search != "" && !strings.Contains(strings.ToLower(string(line)), search) {
			return false
		}
		out.Entries = append(out.Entries, entry)
		return len(out.Entries) >= filter.Limit
	}
	for _, f := range files {
		info, err := f.Stat()
		if err != nil {
			return out, err
		}
		offset := info.Size()
		var pending []byte
		for offset > 0 {
			if out.ScannedBytes >= scanLimit {
				out.Truncated = true
				return out, nil
			}
			n := min(int64(64<<10), offset, scanLimit-out.ScannedBytes)
			offset -= n
			buf := make([]byte, n)
			count, err := f.ReadAt(buf, offset)
			if err != nil && err != io.EOF {
				return out, err
			}
			out.ScannedBytes += int64(count)
			buf = append(buf[:count], pending...)
			end := len(buf)
			for end > 0 {
				index := bytes.LastIndexByte(buf[:end], '\n')
				if index < 0 {
					break
				}
				if consume(buf[index+1 : end]) {
					out.Truncated = true
					return out, nil
				}
				end = index
			}
			pending = append([]byte(nil), buf[:end]...)
			if len(pending) > 2<<20 {
				pending = nil
			} // damaged or oversized external line
		}
		if consume(pending) {
			out.Truncated = true
			return out, nil
		}
	}
	return out, nil
}
