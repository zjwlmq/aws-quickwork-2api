package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAccountPaginationFiltersCountsAndClamps(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for i := range 25 {
		a := Account{Email: fmt.Sprintf("member%02d@example.com", i), TenantURL: "https://tenant.example", AccessToken: "test-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
		switch i % 5 {
		case 1:
			a.Disabled = true
		case 2:
			a.LastError = "test error"
		case 3:
			a.ExpiresAt = 1
		case 4:
			a.AccessToken = ""
		}
		if i == 0 {
			a.DisplayName = "100%_literal"
		}
		if err := st.Save(ctx, &a); err != nil {
			t.Fatal(err)
		}
	}
	page, err := st.ListAccountPage(ctx, AccountFilter{Page: 2, PageSize: 10})
	if err != nil || page.Total != 25 || len(page.Accounts) != 10 || page.Accounts[0].ID != 11 || page.Pages != 3 {
		t.Fatalf("page: %+v %v", page, err)
	}
	if page.Summary != (AccountSummary{Total: 25, Ready: 5, Attention: 15}) {
		t.Fatalf("summary: %+v", page.Summary)
	}
	page, err = st.ListAccountPage(ctx, AccountFilter{Page: 999, PageSize: 10, Status: "error"})
	if err != nil || page.Total != 10 || page.Page != 1 || len(page.Accounts) != 10 || page.Summary.Total != 25 {
		t.Fatalf("filtered/clamped: %+v %v", page, err)
	}
	for search, want := range map[string]int{"MEMBER0": 10, "%_": 1, "not-present": 0} {
		page, err = st.ListAccountPage(ctx, AccountFilter{Page: 8, PageSize: 20, Search: search})
		if err != nil || page.Total != want || page.Page != 1 {
			t.Fatalf("search %q: %+v %v", search, page, err)
		}
	}
}

func TestAccountImportPreservesIdentityStatisticsAndDisabledState(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	original := Account{Email: "member@example.com", TenantURL: "https://tenant.example", AccessToken: "old", UseCount: 19, LastUsedAt: 123, CreatedAt: 100, Disabled: true}
	if err := st.Save(ctx, &original); err != nil {
		t.Fatal(err)
	}
	incoming := original
	incoming.Email = "MEMBER@example.com"
	incoming.AccessToken = "new"
	incoming.ID, incoming.UseCount, incoming.LastUsedAt, incoming.CreatedAt = 999, 999, 999, 999
	result, err := st.ImportAccounts(ctx, []Account{incoming}, false)
	if err != nil || result.Skipped != 1 || result.Updated != 0 {
		t.Fatalf("skip: %+v %v", result, err)
	}
	result, err = st.ImportAccounts(ctx, []Account{incoming, incoming}, true)
	if err != nil || result.Updated != 1 || result.Skipped != 1 {
		t.Fatalf("update: %+v %v", result, err)
	}
	saved, err := st.Get(ctx, original.ID)
	if err != nil || saved.AccessToken != "new" || !saved.Disabled || saved.UseCount != 19 || saved.LastUsedAt != 123 || saved.CreatedAt != 100 {
		t.Fatalf("local state changed: %+v %v", saved, err)
	}
	newAccount := incoming
	newAccount.Email = "other@example.com"
	result, err = st.ImportAccounts(ctx, []Account{newAccount}, false)
	if err != nil || result.Added != 1 {
		t.Fatalf("add: %+v %v", result, err)
	}
	added, _ := st.Get(ctx, result.IDs[0])
	if !added.Disabled || added.UseCount != 0 || added.LastUsedAt != 0 || added.ID == 999 {
		t.Fatalf("imported local fields: %+v", added)
	}
}

func TestAccountImportRollsBackOnDatabaseFailure(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	_, err := st.db.Exec(`CREATE TRIGGER reject_fixture BEFORE INSERT ON accounts WHEN NEW.email='reject@example.com' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.ImportAccounts(ctx, []Account{{Email: "good@example.com"}, {Email: "reject@example.com"}}, false)
	if err == nil {
		t.Fatal("expected database failure")
	}
	list, err := st.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("partial import remained: %d %v", len(list), err)
	}
}
