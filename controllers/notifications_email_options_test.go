package controllers

import (
	"strings"
	"testing"
)

func TestCreateRequest_EmailOptions(t *testing.T) {
	tests := []struct {
		name    string
		req     createNotificationRequest
		wantErr string
	}{
		{"empty is fine", createNotificationRequest{}, ""},
		{"valid reply-to and name", createNotificationRequest{EmailReplyTo: "so-abc@reply.stonesuite.app", EmailFromName: "Harish (Acme)"}, ""},
		{"threading ids", createNotificationRequest{EmailInReplyTo: "<a@b>", EmailReferences: "<a@b> <c@d>"}, ""},
		{"reply-to not an address", createNotificationRequest{EmailReplyTo: "not-an-address"}, "emailReplyTo must be a valid email address."},
		{"reply-to header injection", createNotificationRequest{EmailReplyTo: "a@b.com\r\nBcc: x@y.com"}, "emailReplyTo must not contain line breaks."},
		{"from name header injection", createNotificationRequest{EmailFromName: "Eve\nBcc: x@y.com"}, "emailFromName must not contain line breaks."},
		{"in-reply-to too long", createNotificationRequest{EmailInReplyTo: strings.Repeat("a", maxEmailMessageIDChars+1)}, "emailInReplyTo is too long."},
		{"from name too long", createNotificationRequest{EmailFromName: strings.Repeat("a", maxEmailFromNameChars+1)}, "emailFromName is too long."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.req.emailOptions()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
				t.Fatalf("expected %q, got %v", tc.wantErr, err)
			}
		})
	}
}
