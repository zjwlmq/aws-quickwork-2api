// Package admin serves the optional authenticated account console.
package admin

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/oauth"
	"quickwork-openai/internal/store"
)

//go:embed web/*
var assets embed.FS

type pendingLogin struct {
	Owner     string
	Flow      *oauth.BrowserLogin
	ExpiresAt int64
}

type Server struct {
	cfg        *config.Config
	store      *store.Store
	manager    *accounts.Manager
	mu         sync.Mutex
	sessions   map[string]*session
	attempts   map[string]loginAttempts
	flows      map[string]pendingLogin
	loginSlots chan struct{}
}

func New(cfg *config.Config, st *store.Store, manager *accounts.Manager) (*Server, error) {
	if !cfg.Admin.Enabled {
		return nil, errors.New("web console is disabled")
	}
	if _, _, _, err := parsePasswordHash(cfg.Admin.PasswordHash); err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, store: st, manager: manager,
		sessions: make(map[string]*session), attempts: make(map[string]loginAttempts),
		flows: make(map[string]pendingLogin), loginSlots: make(chan struct{}, 2)}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/api/login", s.login)
	mux.HandleFunc("GET /admin/api/session", s.requireSession(s.sessionInfo))
	mux.HandleFunc("POST /admin/api/logout", s.requireSession(s.logout))
	mux.HandleFunc("GET /admin/api/accounts", s.requireSession(s.listAccounts))
	mux.HandleFunc("POST /admin/api/accounts/{id}/refresh", s.requireSession(s.refreshAccount))
	mux.HandleFunc("PATCH /admin/api/accounts/{id}", s.requireSession(s.toggleAccount))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", s.requireSession(s.deleteAccount))
	mux.HandleFunc("GET /admin/api/connection", s.requireSession(s.connection))
	mux.HandleFunc("POST /admin/api/login-flows", s.requireSession(s.createFlow))
	mux.HandleFunc("POST /admin/api/login-flows/{id}/callback", s.requireSession(s.submitCallback))
	mux.HandleFunc("DELETE /admin/api/login-flows/{id}", s.requireSession(s.cancelFlow))
	mux.HandleFunc("/admin/", s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, code int, message string) {
	reply(w, code, map[string]string{"error": message})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "请求内容无效或超过大小限制")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, http.StatusBadRequest, "请求必须是一个 JSON 对象")
		return false
	}
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.cfg.Admin.PublicURL {
		fail(w, http.StatusForbidden, "请求来源无效")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.Username) > 128 || len(input.Password) > 1024 {
		fail(w, http.StatusBadRequest, "用户名或密码过长")
		return
	}
	ip := requestIP(r)
	if !s.allowLogin(ip) {
		w.Header().Set("Retry-After", "900")
		fail(w, http.StatusTooManyRequests, "尝试次数过多，请在 15 分钟后重试")
		return
	}
	select {
	case s.loginSlots <- struct{}{}:
		defer func() { <-s.loginSlots }()
	default:
		fail(w, http.StatusTooManyRequests, "登录繁忙，请稍后重试")
		return
	}
	valid := verifyPassword(s.cfg.Admin.PasswordHash, input.Password)
	if !valid || input.Username != s.cfg.Admin.Username {
		fail(w, http.StatusUnauthorized, "用户名或密码不正确")
		return
	}
	token, err := randomSecret()
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法创建登录会话")
		return
	}
	csrf, err := randomSecret()
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法创建登录会话")
		return
	}
	sess := &session{ID: sessionKey(token), CSRF: csrf, Expires: time.Now().Add(sessionLifetime)}
	s.mu.Lock()
	for key, old := range s.sessions {
		if time.Now().After(old.Expires) {
			delete(s.sessions, key)
		}
	}
	if len(s.sessions) >= 64 {
		s.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "登录会话过多，请稍后重试")
		return
	}
	s.sessions[sess.ID] = sess
	delete(s.attempts, ip)
	s.mu.Unlock()
	s.setCookie(w, token, int(sessionLifetime.Seconds()))
	s.sessionInfo(w, r, sess)
}

func (s *Server) sessionInfo(w http.ResponseWriter, r *http.Request, sess *session) {
	reply(w, http.StatusOK, map[string]any{"username": s.cfg.Admin.Username, "csrf_token": sess.CSRF, "expires_at": sess.Expires.Unix()})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess.ID)
	for id, flow := range s.flows {
		if flow.Owner == sess.ID {
			delete(s.flows, id)
		}
	}
	s.mu.Unlock()
	s.setCookie(w, "", -1)
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}

type accountView struct {
	ID         int64  `json:"id"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Region     string `json:"region"`
	ExpiresAt  int64  `json:"expires_at"`
	UseCount   int64  `json:"use_count"`
	LastUsedAt int64  `json:"last_used_at"`
	Disabled   bool   `json:"disabled"`
	Status     string `json:"status"`
	LastError  string `json:"last_error"`
}

func view(a store.Account) accountView {
	status := "ready"
	switch {
	case a.Disabled:
		status = "disabled"
	case a.LastError != "":
		status = "error"
	case a.JWT() == "":
		status = "error"
	case a.ExpiresAt > 0 && a.ExpiresAt <= time.Now().Unix():
		status = "expired"
	}
	lastError := a.LastError
	for _, secret := range []string{a.AccessToken, a.RefreshToken, a.IDToken, a.ClientSecret} {
		if secret != "" {
			lastError = strings.ReplaceAll(lastError, secret, "[已隐藏]")
		}
	}
	name := a.DisplayName
	if name == "" {
		name = a.AccountName
	}
	return accountView{ID: a.ID, Email: a.Email, Name: name, Provider: a.Provider, Region: a.Region,
		ExpiresAt: a.ExpiresAt, UseCount: a.UseCount, LastUsedAt: a.LastUsedAt,
		Disabled: a.Disabled, Status: status, LastError: lastError}
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request, sess *session) {
	list, err := s.store.List(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "读取账号失败")
		return
	}
	out := make([]accountView, 0, len(list))
	for _, a := range list {
		out = append(out, view(a))
	}
	reply(w, http.StatusOK, map[string]any{"accounts": out})
}

func accountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, http.StatusBadRequest, "无效的账号 ID")
		return 0, false
	}
	return id, true
}

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request, sess *session) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	a, err := s.manager.RefreshOne(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "账号不存在")
	} else if err != nil {
		fail(w, http.StatusBadGateway, "账号刷新失败，请重新授权或查看账号错误信息")
	} else {
		reply(w, http.StatusOK, view(*a))
	}
}

func (s *Server) toggleAccount(w http.ResponseWriter, r *http.Request, sess *session) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	var input struct {
		Disabled *bool `json:"disabled"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Disabled == nil {
		fail(w, http.StatusBadRequest, "请指定账号启用状态")
		return
	}
	if _, err := s.store.Get(r.Context(), id); errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "账号不存在")
		return
	} else if err != nil {
		fail(w, http.StatusInternalServerError, "读取账号失败")
		return
	}
	if err := s.store.SetDisabled(r.Context(), id, *input.Disabled); err != nil {
		fail(w, http.StatusInternalServerError, "更新账号失败")
		return
	}
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, sess *session) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	deleted, err := s.store.Delete(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, "删除账号失败")
	} else if !deleted {
		fail(w, http.StatusNotFound, "账号不存在")
	} else {
		reply(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Server) connection(w http.ResponseWriter, r *http.Request, sess *session) {
	reply(w, http.StatusOK, map[string]any{"base_url": s.cfg.Admin.PublicURL + "/v1", "models": s.cfg.Models, "api_keys": s.cfg.APIKeys})
}

func (s *Server) createFlow(w http.ResponseWriter, r *http.Request, sess *session) {
	var input struct {
		Provider string `json:"provider"`
		Region   string `json:"region"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if input.Provider == "" {
		input.Provider = "social"
	}
	if input.Provider != "social" && input.Provider != "enterprise" {
		fail(w, http.StatusBadRequest, "请选择社交账号或企业 SSO")
		return
	}
	if input.Provider == "enterprise" && input.Region == "" {
		input.Region = "us-east-1"
	}
	if len(input.Region) > 64 || strings.ContainsAny(input.Region, "/\\?#@: \t\r\n") {
		fail(w, http.StatusBadRequest, "AWS 区域格式无效")
		return
	}
	flow, err := oauth.NewBrowserLogin(oauth.LoginOptions{Provider: input.Provider, Region: input.Region,
		Stage: s.cfg.OAuth.BuildStage, Port: s.cfg.OAuth.CallbackPort, Timeout: 30 * time.Minute, AppVersion: s.cfg.AppVersion}, s.store)
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法创建授权流程")
		return
	}
	id, err := randomSecret()
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法创建授权流程")
		return
	}
	s.mu.Lock()
	for key, old := range s.flows {
		if old.ExpiresAt <= time.Now().Unix() || old.Owner == sess.ID {
			delete(s.flows, key)
		}
	}
	if len(s.flows) >= 64 {
		s.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "授权流程过多，请稍后重试")
		return
	}
	s.flows[id] = pendingLogin{Owner: sess.ID, Flow: flow, ExpiresAt: time.Now().Add(30 * time.Minute).Unix()}
	s.mu.Unlock()
	reply(w, http.StatusCreated, map[string]any{"id": id, "state": flow.State()})
}

func (s *Server) ownedFlow(w http.ResponseWriter, r *http.Request, sess *session) (pendingLogin, bool) {
	s.mu.Lock()
	flow, exists := s.flows[r.PathValue("id")]
	s.mu.Unlock()
	if !exists || flow.Owner != sess.ID {
		fail(w, http.StatusNotFound, "授权流程不存在，请重新添加账号")
		return pendingLogin{}, false
	}
	return flow, true
}

func (s *Server) submitCallback(w http.ResponseWriter, r *http.Request, sess *session) {
	flow, ok := s.ownedFlow(w, r, sess)
	if !ok {
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	state, err := flow.Flow.Submit(ctx, input.URL)
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Sprintf("授权未完成：%s", err))
		return
	}
	reply(w, http.StatusOK, state)
}

func (s *Server) cancelFlow(w http.ResponseWriter, r *http.Request, sess *session) {
	if _, ok := s.ownedFlow(w, r, sess); !ok {
		return
	}
	s.mu.Lock()
	delete(s.flows, r.PathValue("id"))
	s.mu.Unlock()
	reply(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var name, contentType string
	switch r.URL.Path {
	case "/admin/":
		name, contentType = "index.html", "text/html; charset=utf-8"
	case "/admin/app.js":
		name, contentType = "app.js", "text/javascript; charset=utf-8"
	case "/admin/styles.css":
		name, contentType = "styles.css", "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	body, err := assets.ReadFile("web/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
