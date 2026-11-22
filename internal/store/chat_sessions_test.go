package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestChatSessionClaimIsExclusiveAndRecoveryRetiresUncertainWork(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	row := &ChatSession{ID: "chat-test", Owner: "owner", Scope: "scope", Model: "smart", ContextHash: "context", ThreadID: "native"}
	if err := st.CreateChatSession(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteChatSession(ctx, row.ID, "prefix", 2); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.ClaimChatSession(ctx, row.ID, 0)
			if err != nil {
				t.Error(err)
			}
			if ok {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("concurrent append owners: %d", winners.Load())
	}
	if err := st.RecoverChatSessions(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetChatSession(ctx, "owner", row.ID)
	if err != nil || got.Phase != "retired" {
		t.Fatalf("uncertain work survived recovery: %+v %v", got, err)
	}
	if _, err = st.GetChatSession(ctx, "other-owner", row.ID); err == nil {
		t.Fatal("cross-key access")
	}
	rows, err := st.ChatCandidates(ctx, "owner", "scope", "smart", "context")
	if err != nil || len(rows) != 0 {
		t.Fatal("retired state was considered reusable")
	}
}
