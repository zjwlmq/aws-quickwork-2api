// Package requestlog keeps one summary per API call and bounded redacted details.
package requestlog

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

type Options struct {
	Directory      string
	MaxRows        int
	MaxFiles       int
	MaxDiskBytes   int64
	MaxMemoryBytes int64
	MaxAge         time.Duration
}

func DefaultOptions(directory string) Options {
	return Options{Directory: directory, MaxRows: 2000, MaxFiles: 100, MaxDiskBytes: 128 << 20, MaxMemoryBytes: 32 << 20, MaxAge: 7 * 24 * time.Hour}
}

type memoryEntry struct {
	id      string
	data    []byte
	expires time.Time
}
type Recorder struct {
	store       *store.Store
	options     Options
	mu          sync.Mutex
	memory      *list.List
	index       map[string]*list.Element
	memoryBytes int64
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
}

func New(st *store.Store, options Options) (*Recorder, error) {
	if st == nil {
		return nil, errors.New("request logger requires storage")
	}
	if options.Directory == "" {
		return nil, errors.New("request log directory is required")
	}
	defaults := DefaultOptions(options.Directory)
	if options.MaxRows <= 0 {
		options.MaxRows = defaults.MaxRows
	}
	if options.MaxFiles <= 0 {
		options.MaxFiles = defaults.MaxFiles
	}
	if options.MaxDiskBytes <= 0 {
		options.MaxDiskBytes = defaults.MaxDiskBytes
	}
	if options.MaxMemoryBytes <= 0 {
		options.MaxMemoryBytes = defaults.MaxMemoryBytes
	}
	if options.MaxAge <= 0 {
		options.MaxAge = defaults.MaxAge
	}
	if err := os.MkdirAll(options.Directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(options.Directory, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.InitRequests(ctx); err != nil {
		return nil, err
	}
	if err := st.RecoverRequests(ctx, time.Now().UnixMilli()); err != nil {
		return nil, err
	}
	r := &Recorder{store: st, options: options, memory: list.New(), index: map[string]*list.Element{}, stop: make(chan struct{}), done: make(chan struct{})}
	if err := r.prune(); err != nil {
		return nil, err
	}
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				if err := r.prune(); err != nil {
					r.report("requestlog.cleanup_failed", err)
				}
			}
		}
	}()
	return r, nil
}
func (r *Recorder) Close()           { r.closeOnce.Do(func() { close(r.stop); <-r.done }) }
func (r *Recorder) Options() Options { return r.options }
func (r *Recorder) report(event string, err error) {
	diagnostics.Event(context.Background(), "error", event, "error", err)
}

func (r *Recorder) forget(id string) {
	if old := r.index[id]; old != nil {
		r.memoryBytes -= int64(len(old.Value.(memoryEntry).data))
		r.memory.Remove(old)
		delete(r.index, id)
	}
}

func (r *Recorder) remember(id string, data []byte, expires time.Time) {
	if old := r.index[id]; old != nil {
		r.memoryBytes -= int64(len(old.Value.(memoryEntry).data))
		r.memory.Remove(old)
		delete(r.index, id)
	}
	e := r.memory.PushFront(memoryEntry{id: id, data: append([]byte(nil), data...), expires: expires})
	r.index[id] = e
	r.memoryBytes += int64(len(data))
	for r.memoryBytes > r.options.MaxMemoryBytes || r.memory.Len() > r.options.MaxRows {
		e := r.memory.Back()
		if e == nil {
			break
		}
		entry := e.Value.(memoryEntry)
		r.memoryBytes -= int64(len(entry.data))
		delete(r.index, entry.id)
		r.memory.Remove(e)
	}
}

func (r *Recorder) persist(summary store.RequestRecord, detail map[string]any) {
	b, err := json.Marshal(detail)
	if err != nil {
		r.report("requestlog.encode_failed", err)
		return
	}
	// Body snapshots are bounded individually; bound the complete export too.
	if len(b) > 2<<20 {
		detail = map[string]any{"record": summary, "body_note": "完整详情超过 2 MiB，仅保留调用概要和诊断关联"}
		b, _ = json.Marshal(detail)
	}
	wantsDisk := summary.Status == "error" || summary.Status == "recovered" || summary.Status == "cancelled"
	r.mu.Lock()
	ttl := min(time.Hour, r.options.MaxAge)
	if wantsDisk {
		ttl = r.options.MaxAge
	}
	r.remember(summary.ID, b, time.Now().Add(ttl))
	if wantsDisk {
		temp, createErr := os.CreateTemp(r.options.Directory, ".request-*")
		if createErr == nil {
			_, createErr = temp.Write(b)
			if closeErr := temp.Close(); createErr == nil {
				createErr = closeErr
			}
			if createErr == nil {
				createErr = os.Rename(temp.Name(), filepath.Join(r.options.Directory, detailName(summary.ID)))
			}
			if createErr != nil {
				_ = os.Remove(temp.Name())
			}
		}
		if createErr == nil {
			summary.ErrorDetail = true
		} else {
			r.report("requestlog.detail_save_failed", createErr)
		}
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.SaveRequest(ctx, summary); err != nil {
		r.report("requestlog.summary_save_failed", err)
	}
	if err := r.prune(); err != nil {
		r.report("requestlog.cleanup_failed", err)
	}
}

func (r *Recorder) List(ctx context.Context, filter store.RequestFilter) ([]store.RequestRecord, int, error) {
	return r.store.ListRequests(ctx, filter)
}
func (r *Recorder) Get(ctx context.Context, id string) (map[string]any, error) {
	if !safeID(id) {
		return nil, os.ErrNotExist
	}
	summary, err := r.store.GetRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	var b []byte
	storage := "summary"
	if e := r.index[id]; e != nil {
		entry := e.Value.(memoryEntry)
		if time.Now().Before(entry.expires) {
			b = append([]byte(nil), entry.data...)
			storage = "memory"
		} else {
			r.forget(id)
		}
	}
	fileInfo, statErr := os.Stat(filepath.Join(r.options.Directory, detailName(id)))
	fileValid := statErr == nil && fileInfo.Mode().IsRegular() && !fileInfo.ModTime().Before(time.Now().Add(-r.options.MaxAge))
	if fileValid && len(b) > 0 && summary.ErrorDetail {
		storage = "disk"
	}
	if summary.ErrorDetail && !fileValid {
		r.forget(id)
		b = nil
		storage = "summary"
	}
	if statErr != nil && !os.IsNotExist(statErr) {
		r.mu.Unlock()
		return nil, statErr
	}
	if len(b) == 0 && fileValid {
		b, err = os.ReadFile(filepath.Join(r.options.Directory, detailName(id)))
		if err == nil {
			storage = "disk"
		} else if !os.IsNotExist(err) {
			r.mu.Unlock()
			return nil, err
		}
	}
	r.mu.Unlock()
	out := map[string]any{}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, err
		}
	}
	if storage == "summary" {
		out["body_note"] = "成功正文未落盘或已从内存清理；调用概要仍可查看"
		if summary.Status == "running" || summary.Status == "queued" || summary.Status == "in_progress" {
			out["body_note"] = "请求仍在处理，完成后可查看本次详情"
		} else if summary.Status != "success" {
			out["body_note"] = "报错详情已清理或未能保存；调用概要仍可查看"
		}
	}
	out["record"] = summary
	out["storage"] = storage
	return out, nil
}

func (r *Recorder) prune() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.index {
		if !time.Now().Before(e.Value.(memoryEntry).expires) {
			r.forget(id)
		}
	}
	entries, err := os.ReadDir(r.options.Directory)
	if err != nil {
		return err
	}
	type fileInfo struct {
		name     string
		size     int64
		modified time.Time
	}
	var files []fileInfo
	var total int64
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") || !safeID(strings.TrimSuffix(entry.Name(), ".json")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, fileInfo{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	count := len(files)
	cutoff := time.Now().Add(-r.options.MaxAge)
	for _, f := range files {
		if count <= r.options.MaxFiles && total <= r.options.MaxDiskBytes && !f.modified.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(r.options.Directory, f.name)); err != nil && !os.IsNotExist(err) {
			return err
		}
		r.forget(strings.TrimSuffix(f.name, ".json"))
		count--
		total -= f.size
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.store.TrimRequests(ctx, r.options.MaxRows)
}
