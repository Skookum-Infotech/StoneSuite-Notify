// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config holds all environment-derived settings for the notify service.
type Config struct {
	Port string
	// CorsOrigins is the allowlist of browser origins permitted to call this
	// service directly (comma-separated in the environment).
	CorsOrigins []string
	DatabaseURL string
	// JWTSecret must match the StoneSuite-Backend's JWT_SECRET so tokens
	// minted at login there validate here too — this service trusts the
	// same session, it does not issue its own.
	JWTSecret string
	// InternalServiceSecret gates the service-to-service notification
	// creation endpoint. Only trusted backend callers (e.g. the main
	// StoneSuite-Backend when a business event fires) know this value.
	InternalServiceSecret string

	// Email channel. Resend is tried first if RESEND_API_KEY is set,
	// otherwise SMTP is used if SMTPHost is set, otherwise the email
	// channel is a no-op (logged) — same fallback order as
	// StoneSuite-Backend's services/email.go.
	ResendAPIKey string
	EmailFrom    string
	// EmailReplyTo, when set, becomes the Reply-To header on every outbound
	// email. Optional: transactional mail with a real reply address scores
	// better with spam filters than a bare no-reply From. Unset ⇒ no header.
	EmailReplyTo string
	// ResendWebhookSecret is the signing secret ("whsec_…") Resend shows when a
	// webhook endpoint is created. It authenticates POST /api/webhooks/resend,
	// which has no other gate. Optional: unset ⇒ the route answers 503.
	ResendWebhookSecret string

	// Inbound customer replies (the Communication tab). Resend receives mail on
	// ReplyEmailDomain and posts email.received to the same webhook as delivery
	// events; this service fetches the mail's content from Resend and forwards it
	// to StoneSuite-Backend at BackendURL, signed with InternalServiceSecret. It
	// must equal StoneSuite-Backend's REPLY_EMAIL_DOMAIN. Optional: either unset
	// ⇒ received mail is acknowledged and dropped.
	ReplyEmailDomain string
	BackendURL       string

	SMTPHost     string
	SMTPPort     string
	SMTPUsername string
	SMTPPassword string

	// Web push channel (VAPID). Keys must be generated once with
	// webpush.GenerateVAPIDKeys and stored here permanently — rotating
	// them invalidates every existing browser subscription.
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	VAPIDSubject    string
}

// Load reads configuration from the environment and validates that the
// required values are present.
func Load() (Config, error) {
	cfg := Config{
		Port:                  getEnv("PORT", "8090"),
		CorsOrigins:           splitCSV(os.Getenv("CORS_ORIGIN")),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		JWTSecret:             os.Getenv("JWT_SECRET"),
		InternalServiceSecret: os.Getenv("INTERNAL_SERVICE_SECRET"),

		ResendAPIKey: os.Getenv("RESEND_API_KEY"),
		EmailFrom:    os.Getenv("EMAIL_FROM"),
		EmailReplyTo: os.Getenv("EMAIL_REPLY_TO"),

		ResendWebhookSecret: os.Getenv("RESEND_WEBHOOK_SECRET"),

		ReplyEmailDomain: strings.ToLower(strings.TrimSpace(os.Getenv("REPLY_EMAIL_DOMAIN"))),
		BackendURL:       strings.TrimRight(strings.TrimSpace(os.Getenv("BACKEND_URL")), "/"),

		SMTPHost:     os.Getenv("SMTP_HOST"),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUsername: os.Getenv("SMTP_USERNAME"),
		SMTPPassword: os.Getenv("SMTP_PASSWORD"),

		VAPIDPublicKey:  os.Getenv("VAPID_PUBLIC_KEY"),
		VAPIDPrivateKey: os.Getenv("VAPID_PRIVATE_KEY"),
		VAPIDSubject:    os.Getenv("VAPID_SUBJECT"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.JWTSecret == "" {
		return Config{}, fmt.Errorf("JWT_SECRET is required")
	}
	if cfg.InternalServiceSecret == "" {
		return Config{}, fmt.Errorf("INTERNAL_SERVICE_SECRET is required")
	}
	// A BACKEND_URL that is not an absolute http(s) URL would silently drop every
	// customer reply at forward time; fail at boot instead.
	if cfg.BackendURL != "" {
		if u, err := url.Parse(cfg.BackendURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("BACKEND_URL must be an absolute http(s) URL, got %q", cfg.BackendURL)
		}
	}

	// Email and push are optional channels: if unconfigured, the service
	// still runs and simply skips that channel (logged), so in-app
	// notifications keep working even before credentials are provisioned.
	return cfg, nil
}

// EmailConfigured reports whether either email provider has a key/host set.
// Note this is not the same as "can send" — see EmailSendable.
func (c Config) EmailConfigured() bool {
	return c.ResendAPIKey != "" || c.SMTPHost != ""
}

// EmailSendable reports whether the email channel can actually deliver: a
// provider AND a sender address. A provider with no EMAIL_FROM sends
// `{"from": ""}` to Resend, which 422s every message — so the create
// endpoint rejects an email request in that state instead of queueing
// deliveries that can only fail.
func (c Config) EmailSendable() bool {
	return c.EmailConfigured() && c.EmailFrom != ""
}

// PushConfigured reports whether VAPID keys are present for web push.
func (c Config) PushConfigured() bool {
	return c.VAPIDPublicKey != "" && c.VAPIDPrivateKey != ""
}

// ResendWebhookConfigured reports whether provider delivery webhooks can be
// verified (a signing secret is set).
func (c Config) ResendWebhookConfigured() bool {
	return c.ResendWebhookSecret != ""
}

// InboundConfigured reports whether a received customer reply can be handled:
// the reply domain to accept mail for, the backend to forward it to, and the
// Resend key that fetches the mail's content.
func (c Config) InboundConfigured() bool {
	return c.ReplyEmailDomain != "" && c.BackendURL != "" && c.ResendAPIKey != ""
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
