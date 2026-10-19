// Command notify runs the StoneSuite notifications service: a small,
// standalone backend that persists in-app notifications and serves them to
// the frontend bell. It trusts sessions minted by StoneSuite-Backend (same
// JWT secret) rather than handling login itself, and accepts notification
// creation from other StoneSuite services over an internal-secret endpoint.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"stonesuite-notify/audit"
	"stonesuite-notify/config"
	"stonesuite-notify/controllers"
	"stonesuite-notify/database"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/emailevents"
	"stonesuite-notify/inbound"
	"stonesuite-notify/middleware"
	"stonesuite-notify/notifications"
	"stonesuite-notify/preferences"
	"stonesuite-notify/pushsubs"
	"stonesuite-notify/workers"
)

func main() {
	// Load environment variables from .env if it exists.
	if err := godotenv.Load(); err != nil {
		log.Println("warning: .env file not found, using system environment variables")
	} else {
		log.Println("Loaded environment variables from .env")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("CRITICAL: config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("CRITICAL: database: %v", err)
	}
	defer pool.Close()

	if err := database.ApplySchema(ctx, pool); err != nil {
		log.Fatalf("CRITICAL: schema: %v", err)
	}

	if !cfg.EmailConfigured() {
		log.Println("notice: no email provider configured (RESEND_API_KEY or SMTP_HOST) — email channel will be skipped")
	}
	if !cfg.ResendWebhookConfigured() {
		log.Println("notice: no RESEND_WEBHOOK_SECRET — Resend delivery webhooks will be rejected (503); provider delivery status will not be tracked")
	}
	if !cfg.InboundConfigured() {
		log.Println("notice: inbound reply forwarding is off (needs REPLY_EMAIL_DOMAIN, BACKEND_URL and RESEND_API_KEY) — email.received webhooks will be acknowledged and dropped")
	}
	if !cfg.PushConfigured() {
		log.Println("notice: no VAPID keys configured (VAPID_PUBLIC_KEY / VAPID_PRIVATE_KEY) — push channel will be skipped")
	}

	store := notifications.NewPGStore(pool)
	pushStore := pushsubs.NewPGStore(pool)
	prefStore := preferences.NewPGStore(pool)
	deliveryStore := deliveries.NewPGStore(pool)
	emailEventsStore := emailevents.NewPGStore(pool)
	auditStore := audit.NewPGStore(pool)

	// One recorder shared by every handler and both workers: audit writes
	// are detached from the request, and Wait (below) flushes anything
	// still in flight before the process exits.
	auditRecorder := audit.NewAsyncRecorder(auditStore)

	handler := controllers.NewHandler(store, prefStore, deliveryStore, auditRecorder, cfg)
	pushHandler := controllers.NewPushHandler(pushStore, auditRecorder, cfg.VAPIDPublicKey)
	prefHandler := controllers.NewPreferencesHandler(prefStore, auditRecorder)
	auditHandler := controllers.NewAuditHandler(auditStore)
	emailEventsHandler := controllers.NewEmailEventsHandler(emailEventsStore, cfg.ResendWebhookSecret)
	if cfg.InboundConfigured() {
		// A customer's reply, received by Resend, is fetched here and forwarded to
		// the backend signed with the secret both services already share.
		emailEventsHandler.WithInbound(inbound.Processor{
			Domain:    cfg.ReplyEmailDomain,
			Fetcher:   inbound.ResendFetcher{APIKey: cfg.ResendAPIKey},
			Forwarder: inbound.HTTPForwarder{BackendURL: cfg.BackendURL, Secret: cfg.InternalServiceSecret},
		})
	}

	workerDeps := workers.NewDeps(store, deliveryStore, pushStore, auditRecorder, cfg)
	go workers.QueueConsumer{Deps: workerDeps}.Run(ctx)
	go workers.RetryWorker{Deps: workerDeps}.Run(ctx)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":false,"message":"database unreachable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true}`))
	})

	// Provider webhook (delivery status + received customer replies): public by
	// necessity (Resend holds no StoneSuite credentials). The Svix signature check
	// inside the handler is its ONLY gate.
	mux.HandleFunc("POST /api/webhooks/resend", emailEventsHandler.Resend)

	requireInternal := middleware.RequireInternalSecret(cfg.InternalServiceSecret)

	// protected composes RequireAuth with a permission check, so every
	// user-facing route below states the permission it needs and none can
	// be registered as merely "authenticated". See middleware/permissions.go
	// for how permissions resolve against StoneSuite-Backend's tokens.
	protected := func(permission string) func(http.Handler) http.Handler {
		return middleware.Protected(cfg.JWTSecret, permission)
	}

	// The notification bell: unread badge, latest dropdown, paged "view
	// all", and the two read-state writes.
	mux.Handle("GET /api/notifications/summary", protected(middleware.PermNotificationRead)(http.HandlerFunc(handler.Summary)))
	mux.Handle("GET /api/notifications", protected(middleware.PermNotificationRead)(http.HandlerFunc(handler.List)))
	mux.Handle("GET /api/notifications/history", protected(middleware.PermNotificationRead)(http.HandlerFunc(handler.History)))
	mux.Handle("POST /api/notifications/read-all", protected(middleware.PermNotificationUpdate)(http.HandlerFunc(handler.MarkAllRead)))
	mux.Handle("POST /api/notifications/{id}/read", protected(middleware.PermNotificationUpdate)(http.HandlerFunc(handler.MarkRead)))

	mux.HandleFunc("GET /api/push/vapid-public-key", pushHandler.VAPIDKey)
	mux.Handle("POST /api/push/subscribe", protected(middleware.PermPushManage)(http.HandlerFunc(pushHandler.Subscribe)))
	mux.Handle("DELETE /api/push/subscribe", protected(middleware.PermPushManage)(http.HandlerFunc(pushHandler.Unsubscribe)))

	mux.Handle("GET /api/preferences", protected(middleware.PermPreferenceRead)(http.HandlerFunc(prefHandler.Get)))
	mux.Handle("PUT /api/preferences", protected(middleware.PermPreferenceUpdate)(http.HandlerFunc(prefHandler.Update)))

	// Tenant-wide routes for a signed-in administrator. These take the
	// tenant from the caller's own token, never the request body, and need
	// an elevated permission that is never implicitly granted.
	mux.Handle("GET /api/admin/tenant-defaults", protected(middleware.PermPreferenceAdmin)(http.HandlerFunc(prefHandler.AdminGetTenantDefaults)))
	mux.Handle("PUT /api/admin/tenant-defaults", protected(middleware.PermPreferenceAdmin)(http.HandlerFunc(prefHandler.AdminSetTenantDefaults)))
	mux.Handle("GET /api/admin/notifications/{id}/deliveries", protected(middleware.PermNotificationAdmin)(http.HandlerFunc(handler.AdminDeliveries)))
	mux.Handle("GET /api/admin/deliveries", protected(middleware.PermNotificationAdmin)(http.HandlerFunc(handler.AdminDeliveriesByStatus)))
	mux.Handle("GET /api/admin/audit-logs", protected(middleware.PermAuditRead)(http.HandlerFunc(auditHandler.List)))

	// Service-to-service routes: no end-user session, gated by the shared
	// internal secret, and each names its tenant explicitly.
	mux.Handle("POST /api/notifications/internal", requireInternal(http.HandlerFunc(handler.Create)))
	mux.Handle("GET /api/notifications/{id}/deliveries", requireInternal(http.HandlerFunc(handler.Deliveries)))
	mux.Handle("GET /api/deliveries", requireInternal(http.HandlerFunc(handler.DeliveriesByStatus)))
	mux.Handle("POST /api/deliveries/email-status", requireInternal(http.HandlerFunc(emailEventsHandler.EmailStatus)))
	mux.Handle("PUT /api/tenant-defaults", requireInternal(http.HandlerFunc(prefHandler.SetTenantDefaults)))
	mux.Handle("GET /api/audit-logs", requireInternal(http.HandlerFunc(auditHandler.ListInternal)))

	globalHandler := withLogging(withCORS(mux, cfg.CorsOrigins))

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           globalHandler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
		// Audit writes are detached from their request, so in-flight
		// entries would be lost if the process exited immediately after
		// the last response.
		auditRecorder.Wait()
	}()

	fmt.Println("===============================================")
	fmt.Println("  StoneSuite Notify service is running!        ")
	fmt.Printf("  Local Endpoint: http://localhost:%s\n", cfg.Port)
	fmt.Println("===============================================")

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("CRITICAL: server: %v", err)
	}
}

// withCORS echoes the request Origin back only if it is in the configured
// allowlist, mirroring StoneSuite-Backend's CORS handling so browser
// requests from the same frontend origins are accepted here too.
func withCORS(next http.Handler, allowedOrigins []string) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Internal-Secret")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLogging logs one line per request: method, path, status, duration.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
