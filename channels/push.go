package channels

import (
	"context"
	"encoding/json"
	"fmt"

	webpush "github.com/SherClockHolmes/webpush-go"

	"stonesuite-notify/config"
)

// PushSubscription is the minimal shape needed to deliver a Web Push
// message, matching what the browser's PushManager.subscribe() returns.
type PushSubscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

type pushPayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Link  string `json:"link,omitempty"`
}

// SendWebPush delivers a notification to a single subscribed browser via
// the Web Push protocol (VAPID). A subscription that the push service
// reports as gone (404/410 — the user uninstalled, cleared site data, or
// revoked permission) is reported back via the bool so the caller can
// prune it; that is not treated as an error.
func SendWebPush(cfg config.Config, sub PushSubscription, title, body, link string) (stale bool, err error) {
	if !cfg.PushConfigured() {
		return false, fmt.Errorf("push channel: VAPID keys not configured")
	}

	payload, err := json.Marshal(pushPayload{Title: title, Body: body, Link: link})
	if err != nil {
		return false, fmt.Errorf("push: encode payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pushRequestTimeout)
	defer cancel()

	resp, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			P256dh: sub.P256dh,
			Auth:   sub.Auth,
		},
	}, &webpush.Options{
		Subscriber:      cfg.VAPIDSubject,
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		TTL:             60,
		HTTPClient:      pushHTTPClient,
	})
	if err != nil {
		return false, fmt.Errorf("push: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 || resp.StatusCode == 410 {
		return true, nil
	}
	if resp.StatusCode >= 300 {
		return false, fmt.Errorf("push: unexpected status %d", resp.StatusCode)
	}
	return false, nil
}
