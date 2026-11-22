// Package config loads and persists the server configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration is a time.Duration encoded as a Go duration string ("5m", "1h").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err != nil {
		return fmt.Errorf("duration must be a string like \"5m\" or seconds")
	}
	*d = Duration(seconds * float64(time.Second))
	return nil
}

// OAuth holds defaults used by the `login` command.
type OAuth struct {
	Provider     string   `json:"provider"`
	BuildStage   string   `json:"build_stage"`
	Region       string   `json:"region"`
	CallbackPort int      `json:"callback_port"`
	Timeout      Duration `json:"timeout"`
}

// Admin configures the optional web console. PasswordHash is a salted PBKDF2
// digest, never a plaintext password. API keys do not grant console access.
type Admin struct {
	Enabled      bool   `json:"enabled"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	PublicURL    string `json:"public_url"`
}

// Config is the server configuration stored in config.json.
type Config struct {
	Addr             string   `json:"addr"`
	Database         string   `json:"database"`
	APIKeys          []string `json:"api_keys"`
	Models           []string `json:"models"`
	DefaultModel     string   `json:"default_model"`
	RequestTimeout   Duration `json:"request_timeout"`
	RefreshBefore    Duration `json:"refresh_before"`
	MaxAttempts      int      `json:"max_attempts"`
	AppVersion       string   `json:"app_version"`
	Surface          string   `json:"surface"`
	OutpostID        string   `json:"outpost_id"`
	WorkingSessionID string   `json:"working_session_id"`
	Timezone         string   `json:"timezone"`
	Locale           string   `json:"locale"`
	ThinkingEffort   string   `json:"thinking_effort"`
	KeepSessions     bool     `json:"keep_sessions"`
	OAuth            OAuth    `json:"oauth"`
	Admin            Admin    `json:"admin"`

	// Path is the absolute config file path; not serialized.
	Path string `json:"-"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Addr:           "127.0.0.1:8080",
		Database:       "data/quickwork.db",
		APIKeys:        []string{},
		Models:         []string{"smart", "fast", "balanced", "auto"},
		DefaultModel:   "smart",
		RequestTimeout: Duration(5 * time.Minute),
		RefreshBefore:  Duration(10 * time.Minute),
		MaxAttempts:    3,
		AppVersion:     "1.2.4660",
		Surface:        "QUICK_WORK_DESKTOP_WINDOWS",
		Timezone:       "UTC",
		Locale:         "en-US",
		OAuth: OAuth{
			Provider:     "social",
			BuildStage:   "prod",
			CallbackPort: 18080,
			Timeout:      Duration(time.Hour),
		},
	}
}

// Load reads configPath, creating it with defaults when it does not exist.
// Relative paths inside the config (database) are resolved against the
// config file's directory so the server behaves the same from any cwd.
func Load(configPath string) (*Config, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = "config.json"
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	body, err := os.ReadFile(abs)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", abs, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := write(abs, &cfg); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	cfg.Path = abs
	applyDefaults(&cfg)
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	if !filepath.IsAbs(cfg.Database) {
		cfg.Database = filepath.Join(filepath.Dir(abs), cfg.Database)
	}
	return &cfg, nil
}

func write(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

func applyDefaults(c *Config) {
	d := Default()
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = d.Addr
	}
	if strings.TrimSpace(c.Database) == "" {
		c.Database = d.Database
	}
	if len(c.Models) == 0 {
		c.Models = d.Models
	}
	if strings.TrimSpace(c.DefaultModel) == "" {
		c.DefaultModel = d.DefaultModel
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.RefreshBefore < 0 {
		c.RefreshBefore = d.RefreshBefore
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if strings.TrimSpace(c.AppVersion) == "" {
		c.AppVersion = d.AppVersion
	}
	if strings.TrimSpace(c.Surface) == "" {
		c.Surface = d.Surface
	}
	if strings.TrimSpace(c.Timezone) == "" {
		c.Timezone = d.Timezone
	}
	if strings.TrimSpace(c.Locale) == "" {
		c.Locale = d.Locale
	}
	if strings.TrimSpace(c.OAuth.Provider) == "" {
		c.OAuth.Provider = d.OAuth.Provider
	}
	if strings.TrimSpace(c.OAuth.BuildStage) == "" {
		c.OAuth.BuildStage = d.OAuth.BuildStage
	}
	if c.OAuth.CallbackPort <= 0 {
		c.OAuth.CallbackPort = d.OAuth.CallbackPort
	}
	if c.OAuth.Timeout <= 0 {
		c.OAuth.Timeout = d.OAuth.Timeout
	}
}

func (c *Config) validate() error {
	if c.Admin.Enabled {
		if c.Admin.Username == "" || c.Admin.PasswordHash == "" {
			return errors.New("admin.username and admin.password_hash are required")
		}
		u, err := url.Parse(c.Admin.PublicURL)
		if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("admin.public_url must be an origin such as https://example.com")
		}
		local := u.Hostname() == "localhost"
		if ip := net.ParseIP(u.Hostname()); ip != nil {
			local = ip.IsLoopback()
		}
		if u.Scheme != "https" && !(u.Scheme == "http" && local) {
			return errors.New("admin.public_url requires HTTPS except on loopback addresses")
		}
		c.Admin.PublicURL = strings.TrimRight(c.Admin.PublicURL, "/")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request_timeout must be positive")
	}
	if c.OAuth.CallbackPort < 0 || c.OAuth.CallbackPort > 65535 {
		return fmt.Errorf("oauth.callback_port must be 0-65535")
	}
	switch strings.ToLower(c.OAuth.Provider) {
	case "social", "enterprise", "gateway", "social_gateway":
	default:
		return fmt.Errorf("oauth.provider must be social, enterprise, gateway or social_gateway")
	}
	switch strings.ToLower(c.OAuth.BuildStage) {
	case "prod", "beta", "integ", "dev":
	default:
		return fmt.Errorf("oauth.build_stage must be prod, beta, integ or dev")
	}
	return nil
}
