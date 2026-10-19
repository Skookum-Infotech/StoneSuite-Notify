package controllers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stonesuite-notify/inbound"
)

const receivedBody = `{"type":"email.received","created_at":"2026-10-16T10:00:00Z","data":{"email_id":"em_in_1","from":"Priya <p@gmail.com>","to":["r-abcdefghijklmnopqrst@reply.example.com"],"subject":"Re: SO-1","message_id":"<m@x>"}}`

// fakeInbound is an InboundProcessor that records what it was given.
type fakeInbound struct {
	outcome inbound.Outcome
	err     error
	got     []inbound.Event
}

func (f *fakeInbound) Process(_ context.Context, ev inbound.Event) (inbound.Outcome, error) {
	f.got = append(f.got, ev)
	return f.outcome, f.err
}

func TestResendWebhook_ReceivedMailGoesToTheInboundProcessor(t *testing.T) {
	store := &fakeEmailEvents{}
	proc := &fakeInbound{outcome: inbound.OutcomeForwarded}
	h := newEvtHandler(store, evtSecret).WithInbound(proc)

	rec := httptest.NewRecorder()
	h.Resend(rec, signedRequest(t, evtSecret, receivedBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(proc.got) != 1 || proc.got[0].Data.EmailID != "em_in_1" || proc.got[0].Data.Subject != "Re: SO-1" {
		t.Fatalf("processor got %+v, want the parsed event", proc.got)
	}
	if len(store.applied) != 0 {
		t.Fatalf("delivery-status store got %d events; inbound mail must not reach it", len(store.applied))
	}
}

func TestResendWebhook_ReceivedMailStatusCodes(t *testing.T) {
	tests := []struct {
		name string
		proc InboundProcessor
		want int
	}{
		{"not configured: acknowledged and dropped", nil, http.StatusOK},
		{"not ours: acknowledged", &fakeInbound{outcome: inbound.OutcomeIgnored}, http.StatusOK},
		{"a retryable failure asks Resend to retry", &fakeInbound{err: &inbound.RetryableError{Err: errors.New("backend is down")}}, http.StatusBadGateway},
		{"a permanent failure is acknowledged, not retried forever", &fakeInbound{err: errors.New("backend rejected the payload")}, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newEvtHandler(&fakeEmailEvents{}, evtSecret)
			if tc.proc != nil {
				h.WithInbound(tc.proc)
			}
			rec := httptest.NewRecorder()
			h.Resend(rec, signedRequest(t, evtSecret, receivedBody))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestResendWebhook_ReceivedMailStillNeedsAValidSignature(t *testing.T) {
	proc := &fakeInbound{outcome: inbound.OutcomeForwarded}
	h := newEvtHandler(&fakeEmailEvents{}, evtSecret).WithInbound(proc)

	req := signedRequest(t, evtSecret, receivedBody)
	req.Body = io.NopCloser(strings.NewReader(strings.Replace(receivedBody, "em_in_1", "em_forged", 1)))
	rec := httptest.NewRecorder()
	h.Resend(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a tampered body", rec.Code)
	}
	if len(proc.got) != 0 {
		t.Fatalf("processor ran %d times for an unverified request", len(proc.got))
	}
}
