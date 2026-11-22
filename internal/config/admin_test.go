package config

import "testing"

func TestAdminRequiresCredentialsAndSecureOrigin(t *testing.T) {
	for _, origin := range []string{"", "http://public.example", "https://example.com/path", "https://user@example.com", "https://example.com?x=1"} {
		cfg := Default()
		cfg.Admin = Admin{Enabled: true, Username: "admin", PasswordHash: "configured-hash", PublicURL: origin}
		if err := cfg.validate(); err == nil {
			t.Errorf("accepted unsafe origin %q", origin)
		}
	}
	for _, origin := range []string{"https://example.com/", "http://127.0.0.1:18084", "http://localhost:18084"} {
		cfg := Default()
		cfg.Admin = Admin{Enabled: true, Username: "admin", PasswordHash: "configured-hash", PublicURL: origin}
		if err := cfg.validate(); err != nil {
			t.Errorf("rejected origin %q: %v", origin, err)
		}
	}
	cfg := Default()
	cfg.Admin = Admin{Enabled: true, Username: "admin", PublicURL: "https://example.com"}
	if err := cfg.validate(); err == nil {
		t.Fatal("enabled console accepted empty password hash")
	}
}
