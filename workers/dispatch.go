// Package workers implements the Queue Consumer and Retry Worker: two
// ticker-polled goroutines that share attemptDelivery to actually send a
// queued email/push notification and record the outcome. Both resolve
// addressing (recipient email, push subscriptions) fresh at send time from
// notifications/push_subsc, never from anything cached on the delivery
// row, so a retry can't act on stale data.
package workers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"stonesuite-notify/audit"
	"stonesuite-notify/channels"
	"stonesuite-notify/config"
	"stonesuite-notify/deliveries"
	"stonesuite-notify/notifications"
	"stonesuite-notify/pushsubs"
)

// Deps bundles everything attemptDelivery needs. SendEmail/SendPush are
// injectable function fields (defaulting to the real channels senders via
// NewDeps) so tests can exercise attemptDelivery without real network
// calls.
type Deps struct {
	Notifications notifications.Store
	Deliveries    deliveries.Store
	PushSubs      pushsubs.Store
	Audit         audit.Recorder
	Config        config.Config
	SendEmail     func(cfg config.Config, to, title, body, link, emailBodyHTML string, attachments []channels.EmailAttachment, opts channels.EmailOptions) (string, error)
	SendPush      func(cfg config.Config, sub channels.PushSubscription, title, body, link string) (stale bool, err error)
}

// NewDeps builds Deps wired to the real email/push senders in the channels
// package — use this from main.go. Tests construct Deps{} directly with
// fake stores and fake SendEmail/SendPush functions.
func NewDeps(notificationsStore notifications.Store, deliveriesStore deliveries.Store, pushSubsStore pushsubs.Store, recorder audit.Recorder, cfg config.Config) Deps {
	return Deps{
		Notifications: notificationsStore,
		Deliveries:    deliveriesStore,
		PushSubs:      pushSubsStore,
		Audit:         recorder,
		Config:        cfg,
		SendEmail:     channels.SendNotificationEmail,
		SendPush:      channels.SendWebPush,
	}
}

// recordOutcome writes an audit entry for a delivery reaching a terminal
// state. Intermediate retries are deliberately not recorded — the delivery
// row itself already carries attempts and last_error, so auditing every
// attempt would bury the outcomes that matter in noise.
func recordOutcome(ctx context.Context, deps Deps, d deliveries.Delivery, action string, fields map[string]any) {
	if deps.Audit == nil {
		return
	}
	fields["channel"] = d.Channel
	fields["recipientUserId"] = d.RecipientUserID
	fields["attempts"] = d.Attempts + 1

	deps.Audit.Record(ctx, audit.Entry{
		TenantID:   d.TenantID,
		ActorType:  audit.ActorWorker,
		Action:     action,
		Resource:   audit.ResourceDelivery,
		ResourceID: d.ID,
		Metadata:   audit.Metadata(fields),
	})
}

// endpointOutcome is one entry in a push delivery's provider_response map,
// keyed by push_subscriptions.id so a retry can skip endpoints that
// already succeeded instead of re-pushing to every device.
type endpointOutcome struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type pushProviderResponse struct {
	Endpoints map[string]endpointOutcome `json:"endpoints"`
}

// attemptDelivery sends one claimed delivery (already flipped to
// 'processing' by the caller's claim query) and records the outcome.
// Shared by QueueConsumer and RetryWorker so claim-then-send behaves
// identically regardless of which ticker picked the row up.
func attemptDelivery(ctx context.Context, deps Deps, d deliveries.Delivery) {
	n, err := deps.Notifications.Get(ctx, d.TenantID, d.NotificationID)
	if err != nil {
		log.Printf("workers: load notification %s for delivery %s: %v", d.NotificationID, d.ID, err)
		markFailedAttempt(ctx, deps, d, fmt.Sprintf("load notification: %v", err), nil)
		return
	}

	switch d.Channel {
	case deliveries.ChannelEmail:
		attemptEmail(ctx, deps, d, *n)
	case deliveries.ChannelPush:
		attemptPush(ctx, deps, d, *n)
	default:
		log.Printf("workers: delivery %s has unsupported channel %q for worker dispatch", d.ID, d.Channel)
	}
}

func attemptEmail(ctx context.Context, deps Deps, d deliveries.Delivery, n notifications.Notification) {
	var attachments []channels.EmailAttachment
	var emailBodyHTML string
	var emailOpts channels.EmailOptions
	att, err := deps.Notifications.GetAttachment(ctx, n.TenantID, n.ID)
	if err != nil {
		log.Printf("workers: load attachment for notification %s: %v", n.ID, err)
	} else if att != nil {
		emailBodyHTML = att.EmailBodyHTML
		emailOpts = channels.EmailOptions{
			ReplyTo:    att.EmailOptions.ReplyTo,
			FromName:   att.EmailOptions.FromName,
			InReplyTo:  att.EmailOptions.InReplyTo,
			References: att.EmailOptions.References,
		}
		if att.FileName != "" {
			attachments = append(attachments, channels.EmailAttachment{FileName: att.FileName, ContentType: att.ContentType, Content: att.Content})
		}
		for _, f := range att.MoreFiles {
			attachments = append(attachments, channels.EmailAttachment{FileName: f.FileName, ContentType: f.ContentType, Content: f.Content})
		}
	}

	providerID, err := deps.SendEmail(deps.Config, n.RecipientEmail, n.Title, n.Body, n.Link, emailBodyHTML, attachments, emailOpts)
	if err != nil {
		log.Printf("workers: email delivery %s: %v", d.ID, err)
		markFailedAttempt(ctx, deps, d, err.Error(), nil)
		return
	}
	if err := deps.Deliveries.MarkSent(ctx, d.ID, nil, providerID); err != nil {
		log.Printf("workers: mark email delivery %s sent: %v", d.ID, err)
	}
	recordOutcome(ctx, deps, d, audit.ActionDeliverySent, map[string]any{})
}

func attemptPush(ctx context.Context, deps Deps, d deliveries.Delivery, n notifications.Notification) {
	subs, err := deps.PushSubs.ListForUser(ctx, d.TenantID, d.RecipientUserID)
	if err != nil {
		log.Printf("workers: list push subscriptions for delivery %s: %v", d.ID, err)
		markFailedAttempt(ctx, deps, d, err.Error(), nil)
		return
	}
	if len(subs) == 0 {
		if err := deps.Deliveries.MarkSkipped(ctx, d.ID, mustMarshalSkipped()); err != nil {
			log.Printf("workers: mark push delivery %s skipped: %v", d.ID, err)
		}
		return
	}

	prior := decodePushResponse(d.ProviderResponse)
	outcomes := map[string]endpointOutcome{}
	allSent := true

	for _, sub := range subs {
		if prev, ok := prior.Endpoints[sub.ID]; ok && prev.Status == "sent" {
			outcomes[sub.ID] = prev // already delivered to this device on a prior attempt
			continue
		}

		stale, err := deps.SendPush(deps.Config, channels.PushSubscription{
			Endpoint: sub.Endpoint, P256dh: sub.P256dh, Auth: sub.Auth,
		}, n.Title, n.Body, n.Link)
		if err != nil {
			log.Printf("workers: push delivery %s to subscription %s: %v", d.ID, sub.ID, err)
			outcomes[sub.ID] = endpointOutcome{Status: "failed", Error: err.Error()}
			allSent = false
			continue
		}
		if stale {
			if err := deps.PushSubs.Delete(ctx, d.TenantID, d.RecipientUserID, sub.Endpoint); err != nil {
				log.Printf("workers: prune stale push subscription %s: %v", sub.ID, err)
			}
			continue // pruned, not a failure — it won't reappear on the next attempt
		}
		outcomes[sub.ID] = endpointOutcome{Status: "sent"}
	}

	responseJSON, _ := json.Marshal(pushProviderResponse{Endpoints: outcomes})

	if allSent {
		if err := deps.Deliveries.MarkSent(ctx, d.ID, responseJSON, ""); err != nil {
			log.Printf("workers: mark push delivery %s sent: %v", d.ID, err)
		}
		recordOutcome(ctx, deps, d, audit.ActionDeliverySent, map[string]any{"endpoints": len(outcomes)})
		return
	}
	markFailedAttempt(ctx, deps, d, "one or more push endpoints failed", responseJSON)
}

// markFailedAttempt records a failed attempt on the delivery row, and — only
// once the attempt budget is spent and the row is therefore terminal —
// writes the audit entry for the failure.
func markFailedAttempt(ctx context.Context, deps Deps, d deliveries.Delivery, lastError string, providerResponse []byte) {
	attempts := d.Attempts + 1

	if err := deps.Deliveries.MarkRetrying(ctx, d.ID, attempts, lastError, providerResponse); err != nil {
		log.Printf("workers: mark delivery %s retrying: %v", d.ID, err)
	}
	if attempts >= deliveries.MaxAttempts {
		// A stable, greppable line for an ops log alert (Fly/Axiom): a
		// delivery has exhausted its retries and will never be sent. The
		// audit row below is the durable record; this is the active signal,
		// since nothing polls the audit trail.
		log.Printf("workers: event=delivery_permanently_failed delivery_id=%s notification_id=%s tenant_id=%s channel=%s recipient_user_id=%s attempts=%d last_error=%q",
			d.ID, d.NotificationID, d.TenantID, d.Channel, d.RecipientUserID, attempts, lastError)
		recordOutcome(ctx, deps, d, audit.ActionDeliveryFailed, map[string]any{"lastError": lastError})
	}
}

func decodePushResponse(raw []byte) pushProviderResponse {
	var resp pushProviderResponse
	if len(raw) == 0 {
		return pushProviderResponse{Endpoints: map[string]endpointOutcome{}}
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Endpoints == nil {
		return pushProviderResponse{Endpoints: map[string]endpointOutcome{}}
	}
	return resp
}

func mustMarshalSkipped() []byte {
	b, _ := json.Marshal(map[string]string{"reason": "no push subscriptions"})
	return b
}
