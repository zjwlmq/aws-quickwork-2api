package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"quickwork-openai/internal/diagnostics"
	"quickwork-openai/internal/store"
)

const (
	accountTransferFormat = "quickwork-accounts"
	maxTransferAccounts   = 1000
	maxTransferBytes      = 16 << 20
)

// Credentials are exposed only by the dedicated, session- and CSRF-protected
// export operation. Account lists and ordinary JSON marshaling remain redacted.
type accountTransfer struct {
	Email         string `json:"email"`
	DisplayName   string `json:"display_name,omitempty"`
	Provider      string `json:"provider"`
	BuildStage    string `json:"build_stage,omitempty"`
	Region        string `json:"region"`
	TenantURL     string `json:"tenant_url"`
	WSHost        string `json:"ws_host"`
	WSPath        string `json:"ws_path,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	IDToken       string `json:"id_token,omitempty"`
	ActorARN      string `json:"actor_arn,omitempty"`
	AWSAccountID  string `json:"aws_account_id,omitempty"`
	AccountName   string `json:"account_name,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	IssuerURL     string `json:"issuer_url,omitempty"`
	IDCRegion     string `json:"idc_region,omitempty"`
	AuthEndpoint  string `json:"auth_endpoint,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	UserInfo      string `json:"user_info,omitempty"`
	ExpiresAt     int64  `json:"expires_at"`
	ObtainedAt    int64  `json:"obtained_at,omitempty"`
	Disabled      bool   `json:"disabled"`
}

type accountDocument struct {
	Format         string            `json:"format,omitempty"`
	Version        int               `json:"version,omitempty"`
	ExportedAt     string            `json:"exported_at,omitempty"`
	Accounts       []accountTransfer `json:"accounts"`
	UpdateExisting bool              `json:"update_existing,omitempty"`
}

func transferFromAccount(a store.Account) accountTransfer {
	return accountTransfer{Email: a.Email, DisplayName: a.DisplayName, Provider: a.Provider,
		BuildStage: a.BuildStage, Region: a.Region, TenantURL: a.TenantURL, WSHost: a.WSHost, WSPath: a.WSPath,
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, IDToken: a.IDToken, ActorARN: a.ActorARN,
		AWSAccountID: a.AWSAccountID, AccountName: a.AccountName, ClientID: a.ClientID, ClientSecret: a.ClientSecret,
		IssuerURL: a.IssuerURL, IDCRegion: a.IDCRegion, AuthEndpoint: a.AuthEndpoint, TokenEndpoint: a.TokenEndpoint,
		RedirectURI: a.RedirectURI, UserInfo: a.UserInfo, ExpiresAt: a.ExpiresAt, ObtainedAt: a.ObtainedAt, Disabled: a.Disabled}
}

func (a accountTransfer) account() store.Account {
	return store.Account{Email: a.Email, DisplayName: a.DisplayName, Provider: a.Provider,
		BuildStage: a.BuildStage, Region: a.Region, TenantURL: a.TenantURL, WSHost: a.WSHost, WSPath: a.WSPath,
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, IDToken: a.IDToken, ActorARN: a.ActorARN,
		AWSAccountID: a.AWSAccountID, AccountName: a.AccountName, ClientID: a.ClientID, ClientSecret: a.ClientSecret,
		IssuerURL: a.IssuerURL, IDCRegion: a.IDCRegion, AuthEndpoint: a.AuthEndpoint, TokenEndpoint: a.TokenEndpoint,
		RedirectURI: a.RedirectURI, UserInfo: a.UserInfo, ExpiresAt: a.ExpiresAt, ObtainedAt: a.ObtainedAt, Disabled: a.Disabled}
}

func accountFilter(w http.ResponseWriter, r *http.Request) (store.AccountFilter, bool) {
	q := r.URL.Query()
	f := store.AccountFilter{Page: 1, PageSize: 20, Search: strings.TrimSpace(q.Get("search")), Status: q.Get("status")}
	for _, item := range []struct {
		name string
		dst  *int
		max  int
	}{{"page", &f.Page, 1_000_000_000}, {"page_size", &f.PageSize, 100}} {
		if value := q.Get(item.name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > item.max {
				fail(w, 400, fmt.Sprintf("%s 必须为 1 到 %d", item.name, item.max))
				return f, false
			}
			*item.dst = n
		}
	}
	switch f.Status {
	case "", "all", "ready", "expired", "error", "disabled":
	default:
		fail(w, 400, "账号状态无效")
		return f, false
	}
	if len(f.Search) > 256 {
		fail(w, 400, "搜索条件过长")
		return f, false
	}
	return f, true
}

func decodeTransfer(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON value")
	}
	return nil
}

func validTransferURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback()))
}

func validateTransfer(a *accountTransfer) string {
	a.Email = strings.TrimSpace(a.Email)
	a.TenantURL = strings.TrimSpace(a.TenantURL)
	a.WSHost = strings.TrimSpace(a.WSHost)
	email, err := mail.ParseAddress(a.Email)
	if err != nil || email.Address != a.Email || len(a.Email) > 254 {
		return "email 格式无效"
	}
	enterprise := a.Provider == "enterprise" || a.Provider == "gateway"
	if !enterprise && a.Provider != "social" && a.Provider != "social_gateway" {
		return "provider 必须为 social、enterprise、gateway 或 social_gateway"
	}
	if !validTransferURL(a.TenantURL) {
		return "tenant_url 必须为有效的 HTTPS 地址"
	}
	host, err := url.Parse("wss://" + a.WSHost)
	if err != nil || host.Hostname() == "" || host.Host != a.WSHost || host.User != nil || host.Path != "" || host.RawQuery != "" || host.Fragment != "" {
		return "ws_host 格式无效"
	}
	if a.WSPath != "" && (!strings.HasPrefix(a.WSPath, "/") || strings.ContainsAny(a.WSPath, "\r\n")) {
		return "ws_path 格式无效"
	}
	if a.AccessToken == "" && a.IDToken == "" && a.RefreshToken == "" {
		return "缺少账号授权 Token"
	}
	if a.RefreshToken != "" && (a.ClientID == "" || (!enterprise && a.ClientSecret == "")) {
		return "刷新授权缺少 client_id 或 client_secret"
	}
	if enterprise && a.RefreshToken != "" && a.TokenEndpoint == "" {
		return "企业账号刷新授权缺少 token_endpoint"
	}
	for _, value := range []string{a.IssuerURL, a.AuthEndpoint, a.TokenEndpoint} {
		if value != "" && !validTransferURL(value) {
			return "OAuth 地址必须为有效的 HTTPS 地址"
		}
	}
	for _, value := range []string{a.Region, a.IDCRegion} {
		if len(value) > 64 || strings.ContainsAny(value, "/\\?#@: \t\r\n") {
			return "AWS 区域格式无效"
		}
	}
	if a.ExpiresAt < 0 || a.ObtainedAt < 0 {
		return "到期或授权时间无效"
	}
	if a.BuildStage == "" {
		a.BuildStage = "prod"
	}
	if a.Region == "" {
		a.Region = "us-east-1"
	}
	return ""
}

func (s *Server) importAccounts(w http.ResponseWriter, r *http.Request, _ *session) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTransferBytes))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			fail(w, 413, "账号文件不能超过 16 MiB")
		} else {
			fail(w, 400, "无法读取账号文件")
		}
		return
	}
	body = bytes.TrimSpace(bytes.TrimPrefix(body, []byte{0xef, 0xbb, 0xbf}))
	var input accountDocument
	if len(body) > 0 && body[0] == '[' {
		err = decodeTransfer(body, &input.Accounts)
	} else {
		err = decodeTransfer(body, &input)
	}
	if err != nil || (input.Format != "" && input.Format != accountTransferFormat) || (input.Version != 0 && input.Version != 1) {
		fail(w, 400, "JSON 格式不受支持，请使用本站导出的账号文件或相同字段的账号数组")
		return
	}
	if len(input.Accounts) == 0 || len(input.Accounts) > maxTransferAccounts {
		fail(w, 400, "每次导入需要包含 1 到 1000 个账号")
		return
	}
	records := make([]store.Account, len(input.Accounts))
	for i := range input.Accounts {
		if message := validateTransfer(&input.Accounts[i]); message != "" {
			fail(w, 400, fmt.Sprintf("第 %d 个账号：%s；本次未写入任何账号", i+1, message))
			return
		}
		records[i] = input.Accounts[i].account()
	}
	result, err := s.store.ImportAccounts(r.Context(), records, input.UpdateExisting)
	if err != nil {
		diagnostics.Event(r.Context(), "error", "admin.accounts.import_failed")
		fail(w, 500, "账号导入失败，本次未写入任何账号")
		return
	}
	s.quotaMu.Lock()
	for _, id := range result.IDs {
		delete(s.quotas, id)
	}
	s.quotaMu.Unlock()
	diagnostics.Event(r.Context(), "info", "admin.accounts.imported", "added", result.Added, "updated", result.Updated, "skipped", result.Skipped)
	reply(w, 200, result)
}

func (s *Server) exportAccounts(w http.ResponseWriter, r *http.Request, _ *session) {
	var input struct {
		IDs []int64 `json:"ids"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.IDs) > maxTransferAccounts {
		fail(w, 400, "每次最多导出 1000 个账号")
		return
	}
	seen := make(map[int64]bool, len(input.IDs))
	for _, id := range input.IDs {
		if id <= 0 || seen[id] {
			fail(w, 400, "账号 ID 无效或重复")
			return
		}
		seen[id] = true
	}
	list, err := s.store.SelectedAccounts(r.Context(), input.IDs, maxTransferAccounts+1)
	if err != nil {
		fail(w, 500, "读取账号失败")
		return
	}
	if len(input.IDs) > 0 && len(list) != len(input.IDs) {
		fail(w, 404, "部分所选账号已不存在，请更新列表后重新选择")
		return
	}
	if len(list) > maxTransferAccounts {
		fail(w, 400, "账号超过 1000 个，请勾选账号后分批导出")
		return
	}
	output := accountDocument{Format: accountTransferFormat, Version: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339), Accounts: make([]accountTransfer, 0, len(list))}
	for _, a := range list {
		output.Accounts = append(output.Accounts, transferFromAccount(a))
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		fail(w, 500, "账号导出失败")
		return
	}
	if len(encoded) > maxTransferBytes {
		fail(w, 400, "导出文件超过 16 MiB，请勾选较少账号后分批导出")
		return
	}
	diagnostics.Event(r.Context(), "info", "admin.accounts.exported", "count", len(list))
	w.Header().Set("Content-Disposition", `attachment; filename="quickwork-accounts-`+time.Now().UTC().Format("20060102-150405")+`.json"`)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(encoded)
}
