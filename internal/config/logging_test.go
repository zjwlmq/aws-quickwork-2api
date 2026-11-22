package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoggingDefaultsAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"addr":"127.0.0.1:18081"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.MaxMB != 20 || cfg.Logging.Backups != 5 || cfg.Logging.File != filepath.Join(dir, "data", "logs", "quickwork.jsonl") {
		t.Fatalf("old configs lost defaults: %+v", cfg.Logging)
	}
	for _, input := range []string{`{"logging":{"level":"everything"}}`, `{"logging":{"max_mb":101}}`, `{"logging":{"backups":-1}}`} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid logging config accepted: %s", input)
		}
	}
}
