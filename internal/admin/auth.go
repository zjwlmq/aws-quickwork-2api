package admin

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const cookieName = "qw_admin_session"
const sessionLifetime = 12 * time.Hour
const rememberedSessionLifetime = 30 * 24 * time.Hour

type session struct {
	ID      string
	CSRF    string
	Expires time.Time
}

type loginAttempts struct {
	Count int
	Until time.Time
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashPassword creates a salted digest suitable for admin.password_hash.
func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("password must contain 12-1024 bytes")
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$600000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(digest), nil
}

func parsePasswordHash(encoded string) (int, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return 0, nil, nil, errors.New("invalid admin password hash")
	}
	iterations, err := strconv.Atoi(parts[1])
	salt, saltErr := base64.RawStdEncoding.DecodeString(parts[2])
	digest, digestErr := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || iterations < 600000 || iterations > 2000000 || saltErr != nil || len(salt) < 16 || digestErr != nil || len(digest) != 32 {
		return 0, nil, nil, errors.New("invalid admin password hash parameters")
	}
	return iterations, salt, digest, nil
}

func verifyPassword(encoded, password string) bool {
	i, salt, want, err := parsePasswordHash(encoded)
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, i, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Server) authVersion() string {
	return sessionKey(s.cfg.Admin.Username + "\x00" + s.cfg.Admin.PasswordHash + "\x00" + s.cfg.Admin.PublicURL)
}

func (s *Server) setCookie(w http.ResponseWriter, value string, maxAge int) {
	expires := time.Unix(1, 0)
	if maxAge > 0 {
		expires = time.Now().Add(time.Duration(maxAge) * time.Second)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: "/admin", HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.Admin.PublicURL, "https://"),
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge, Expires: expires,
	})
}

func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request, *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil || len(cookie.Value) != 43 {
			fail(w, http.StatusUnauthorized, "请先登录管理员账号")
			return
		}
		key := sessionKey(cookie.Value)
		saved, err := s.store.GetAdminSession(r.Context(), key, s.authVersion(), time.Now().Unix())
		if errors.Is(err, sql.ErrNoRows) {
			s.setCookie(w, "", -1)
			fail(w, http.StatusUnauthorized, "登录已过期，请重新登录")
			return
		}
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "登录状态暂时无法读取，请稍后重试")
			return
		}
		sess := &session{ID: saved.ID, CSRF: saved.CSRF, Expires: time.Unix(saved.ExpiresAt, 0)}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			csrf := r.Header.Get("X-CSRF-Token")
			if r.Header.Get("Origin") != s.cfg.Admin.PublicURL || subtle.ConstantTimeCompare([]byte(csrf), []byte(sess.CSRF)) != 1 {
				fail(w, http.StatusForbidden, "请求校验失败，请刷新页面后重试")
				return
			}
		}
		next(w, r, sess)
	}
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if real := net.ParseIP(r.Header.Get("X-Real-IP")); real != nil {
			return real.String()
		}
	}
	return host
}

func (s *Server) allowLogin(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for key, attempts := range s.attempts {
		if now.After(attempts.Until) {
			delete(s.attempts, key)
		}
	}
	a := s.attempts[ip]
	if a.Count >= 5 || (a.Count == 0 && len(s.attempts) >= 10000) {
		return false
	}
	if a.Count == 0 {
		a.Until = now.Add(15 * time.Minute)
	}
	a.Count++
	s.attempts[ip] = a
	return true
}
