package store

import (
	"bytes"
	"context"
	"testing"
)

func TestMediaSigningPersistenceAndVideoRecovery(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.InitMedia(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := st.MediaSecret(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.MediaSecret(ctx)
	if err != nil || len(first) != 32 || !bytes.Equal(first, second) {
		t.Fatal("download signing secret changed")
	}
	for _, j := range []VideoJob{
		{ID: "running", Owner: "one", Prompt: "test", Model: "smart", Status: "in_progress", Seconds: "3", Size: "1280x720", InputJSON: "private reference image"},
		{ID: "done", Owner: "two", Prompt: "test", Model: "smart", Status: "completed", Seconds: "3", Size: "1280x720"},
	} {
		if err = st.CreateVideo(ctx, &j); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.RecoverVideos(ctx); err != nil {
		t.Fatal(err)
	}
	running, err := st.GetVideo(ctx, "running")
	if err != nil || running.Status != "failed" || running.CompletedAt == 0 || running.InputJSON != "[]" {
		t.Fatal("interrupted job not recovered")
	}
	done, err := st.GetVideo(ctx, "done")
	if err != nil || done.Status != "completed" {
		t.Fatal("completed job changed")
	}
	jobs, err := st.ListVideos(ctx, "one")
	if err != nil || len(jobs) != 1 || jobs[0].ID != "running" {
		t.Fatal("owner isolation failed")
	}
}
