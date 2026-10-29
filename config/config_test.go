package config

import "testing"

func TestEmailSendable(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"nothing set", Config{}, false},
		{"resend key but no EMAIL_FROM", Config{ResendAPIKey: "re_x"}, false},
		{"smtp host but no EMAIL_FROM", Config{SMTPHost: "smtp.x"}, false},
		{"EMAIL_FROM but no provider", Config{EmailFrom: "a@b.com"}, false},
		{"resend key + EMAIL_FROM", Config{ResendAPIKey: "re_x", EmailFrom: "a@b.com"}, true},
		{"smtp host + EMAIL_FROM", Config{SMTPHost: "smtp.x", EmailFrom: "a@b.com"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EmailSendable(); got != tc.want {
				t.Fatalf("EmailSendable() = %v, want %v", got, tc.want)
			}
			// EmailConfigured stays the looser "a provider key is present" check.
			if tc.cfg.EmailSendable() && !tc.cfg.EmailConfigured() {
				t.Fatal("EmailSendable implies EmailConfigured")
			}
		})
	}
}

func TestInboundConfigured(t *testing.T) {
	full := Config{ReplyEmailDomain: "reply.example.com", BackendURL: "https://api.example.com", ResendAPIKey: "re_x"}
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"nothing set", Config{}, false},
		{"all three set", full, true},
		{"no reply domain", Config{BackendURL: full.BackendURL, ResendAPIKey: full.ResendAPIKey}, false},
		{"no backend url", Config{ReplyEmailDomain: full.ReplyEmailDomain, ResendAPIKey: full.ResendAPIKey}, false},
		{"no resend key", Config{ReplyEmailDomain: full.ReplyEmailDomain, BackendURL: full.BackendURL}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.InboundConfigured(); got != tc.want {
				t.Fatalf("InboundConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoad_InboundSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("JWT_SECRET", "jwt")
	t.Setenv("INTERNAL_SERVICE_SECRET", "internal")

	t.Run("domain is lower-cased and the url loses its trailing slash", func(t *testing.T) {
		t.Setenv("REPLY_EMAIL_DOMAIN", "  Reply.Example.COM ")
		t.Setenv("BACKEND_URL", " https://api.example.com/ ")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.ReplyEmailDomain != "reply.example.com" || cfg.BackendURL != "https://api.example.com" {
			t.Fatalf("got %q / %q", cfg.ReplyEmailDomain, cfg.BackendURL)
		}
	})

	t.Run("both unset is fine: the feature is just off", func(t *testing.T) {
		t.Setenv("REPLY_EMAIL_DOMAIN", "")
		t.Setenv("BACKEND_URL", "")
		if _, err := Load(); err != nil {
			t.Fatalf("Load: %v", err)
		}
	})

	for _, bad := range []string{"api.example.com", "ftp://api.example.com", "https://", "://nope"} {
		t.Run("rejects BACKEND_URL "+bad, func(t *testing.T) {
			t.Setenv("BACKEND_URL", bad)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted BACKEND_URL=%q, want an error", bad)
			}
		})
	}
}

func TestResendWebhookConfigured(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"unset", Config{}, false},
		{"set", Config{ResendWebhookSecret: "whsec_abc"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ResendWebhookConfigured(); got != tc.want {
				t.Fatalf("ResendWebhookConfigured() = %v, want %v", got, tc.want)
			}
		})
	}
}
