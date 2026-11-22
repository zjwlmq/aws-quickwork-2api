package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestAdminSessionCommitSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InitAdminSessions(ctx, "version-1", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveAdminSession(ctx, AdminSession{ID: "hashed-cookie", CSRF: "csrf-value", ExpiresAt: 200, AuthVersion: "version-1"}, 100); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	saved, err := st.GetAdminSession(ctx, "hashed-cookie", "version-1", 101)
	if err != nil || saved.CSRF != "csrf-value" {
		t.Fatalf("committed session lost: %+v %v", saved, err)
	}
	if _, err := st.GetAdminSession(ctx, "hashed-cookie", "version-1", 200); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("expired session accepted")
	}
	if err := st.InitAdminSessions(ctx, "version-2", 101); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAdminSession(ctx, "hashed-cookie", "version-1", 101); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("credential version change did not remove old sessions")
	}
}
