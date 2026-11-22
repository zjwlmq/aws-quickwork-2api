package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"time"
)

type MediaFile struct {
	ID         string `json:"id"`
	Owner      string `json:"-"`
	AccountID  int64  `json:"-"`
	ThreadID   string `json:"-"`
	NativePath string `json:"-"`
	Filename   string `json:"filename"`
	MIMEType   string `json:"mime_type"`
	Size       int64  `json:"bytes"`
	CreatedAt  int64  `json:"created_at"`
	URL        string `json:"url,omitempty"`
}

type VideoJob struct {
	ID          string `json:"id"`
	Owner       string `json:"-"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model"`
	Status      string `json:"status"`
	Seconds     string `json:"seconds"`
	Size        string `json:"size"`
	CreatedAt   int64  `json:"created_at"`
	CompletedAt int64  `json:"completed_at,omitempty"`
	Error       string `json:"error,omitempty"`
	FileID      string `json:"file_id,omitempty"`
	Result      string `json:"result,omitempty"`
	InputJSON   string `json:"-"`
	Object      string `json:"object"`
	URL         string `json:"url,omitempty"`
}

func (s *Store) InitMedia(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS media_files(id TEXT PRIMARY KEY, owner TEXT NOT NULL, account_id INTEGER NOT NULL, thread_id TEXT NOT NULL,native_path TEXT NOT NULL,filename TEXT NOT NULL,mime_type TEXT NOT NULL,size INTEGER NOT NULL,created_at INTEGER NOT NULL,UNIQUE(owner,account_id,thread_id,native_path));
CREATE TABLE IF NOT EXISTS video_jobs(id TEXT PRIMARY KEY,owner TEXT NOT NULL,prompt TEXT NOT NULL,model TEXT NOT NULL,status TEXT NOT NULL,seconds TEXT NOT NULL,size TEXT NOT NULL,created_at INTEGER NOT NULL,completed_at INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',file_id TEXT NOT NULL DEFAULT '',result TEXT NOT NULL DEFAULT '',input_json TEXT NOT NULL DEFAULT '[]');
CREATE TABLE IF NOT EXISTS media_secrets(name TEXT PRIMARY KEY,value TEXT NOT NULL);`)
	return err
}

func (s *Store) MediaSecret(ctx context.Context) ([]byte, error) {
	var value string
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO media_secrets(name,value) VALUES('signing',?)`, base64.RawStdEncoding.EncodeToString(data)); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM media_secrets WHERE name='signing'`).Scan(&value); err != nil {
		return nil, err
	}
	return base64.RawStdEncoding.DecodeString(value)
}

func (s *Store) SaveMedia(ctx context.Context, f *MediaFile) error {
	if f.CreatedAt == 0 {
		f.CreatedAt = time.Now().Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO media_files(id,owner,account_id,thread_id,native_path,filename,mime_type,size,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, f.ID, f.Owner, f.AccountID, f.ThreadID, f.NativePath, f.Filename, f.MIMEType, f.Size, f.CreatedAt)
	return err
}
func (s *Store) GetMedia(ctx context.Context, id string) (*MediaFile, error) {
	var f MediaFile
	err := s.db.QueryRowContext(ctx, `SELECT id,owner,account_id,thread_id,native_path,filename,mime_type,size,created_at FROM media_files WHERE id=?`, id).Scan(&f.ID, &f.Owner, &f.AccountID, &f.ThreadID, &f.NativePath, &f.Filename, &f.MIMEType, &f.Size, &f.CreatedAt)
	return &f, err
}
func (s *Store) FindMedia(ctx context.Context, owner string, account int64, thread, nativePath string) (*MediaFile, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM media_files WHERE owner=? AND account_id=? AND thread_id=? AND native_path=?`, owner, account, thread, nativePath).Scan(&id); err != nil {
		return nil, err
	}
	return s.GetMedia(ctx, id)
}
func (s *Store) CreateVideo(ctx context.Context, j *VideoJob) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO video_jobs(id,owner,prompt,model,status,seconds,size,created_at,input_json) VALUES(?,?,?,?,?,?,?,?,?)`, j.ID, j.Owner, j.Prompt, j.Model, j.Status, j.Seconds, j.Size, j.CreatedAt, j.InputJSON)
	return err
}
func (s *Store) UpdateVideo(ctx context.Context, j *VideoJob) error {
	_, err := s.db.ExecContext(ctx, `UPDATE video_jobs SET status=?,completed_at=?,error=?,file_id=?,result=?,input_json=? WHERE id=?`, j.Status, j.CompletedAt, j.Error, j.FileID, j.Result, j.InputJSON, j.ID)
	return err
}

const videoColumns = `id,owner,prompt,model,status,seconds,size,created_at,completed_at,error,file_id,result,input_json`

func scanVideo(row scanner) (*VideoJob, error) {
	j := VideoJob{Object: "video"}
	err := row.Scan(&j.ID, &j.Owner, &j.Prompt, &j.Model, &j.Status, &j.Seconds, &j.Size, &j.CreatedAt, &j.CompletedAt, &j.Error, &j.FileID, &j.Result, &j.InputJSON)
	return &j, err
}
func (s *Store) GetVideo(ctx context.Context, id string) (*VideoJob, error) {
	return scanVideo(s.db.QueryRowContext(ctx, `SELECT `+videoColumns+` FROM video_jobs WHERE id=?`, id))
}
func (s *Store) ListVideos(ctx context.Context, owner string) ([]VideoJob, error) {
	q := `SELECT ` + videoColumns + ` FROM video_jobs`
	var args []any
	if owner != "" {
		q += ` WHERE owner=?`
		args = append(args, owner)
	}
	q += ` ORDER BY created_at DESC LIMIT 100`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]VideoJob, 0)
	for rows.Next() {
		j, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}
func (s *Store) RecoverVideos(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE video_jobs SET status='failed',error='服务重启，生成任务已中断，请重新提交',completed_at=?,input_json='[]' WHERE status IN ('queued','in_progress')`, time.Now().Unix())
	return err
}
