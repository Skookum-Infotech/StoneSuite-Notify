package emailevents

import (
	"encoding/base64"
	"errors"
	"strconv"
	"testing"
	"time"
)

var testSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("test-signing-key-0123456789abcdef"))

func TestVerifySignature(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	body := []byte(`{"type":"email.bounced","data":{"email_id":"em_1"}}`)
	good, err := SignatureHeader(testSecret, "msg_1", ts, body)
	if err != nil {
		t.Fatalf("SignatureHeader: %v", err)
	}
	otherSecret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("a-completely-different-key-000000"))
	wrongSig, err := SignatureHeader(otherSecret, "msg_1", ts, body)
	if err != nil {
		t.Fatalf("SignatureHeader(other): %v", err)
	}

	tests := []struct {
		name      string
		secret    string
		msgID     string
		timestamp string
		sigs      string
		body      []byte
		now       time.Time
		wantErr   error
	}{
		{"valid", testSecret, "msg_1", ts, good, body, now, nil},
		{"valid with a second, wrong signature first", testSecret, "msg_1", ts, wrongSig + " " + good, body, now, nil},
		{"valid with the wrong signature last", testSecret, "msg_1", ts, good + " " + wrongSig, body, now, nil},
		{"tampered body", testSecret, "msg_1", ts, good, []byte(`{"type":"email.delivered"}`), now, ErrInvalidSignature},
		{"different message id", testSecret, "msg_2", ts, good, body, now, ErrInvalidSignature},
		{"signed with another secret", testSecret, "msg_1", ts, wrongSig, body, now, ErrInvalidSignature},
		{"timestamp too old", testSecret, "msg_1", ts, good, body, now.Add(6 * time.Minute), ErrInvalidSignature},
		{"timestamp too far in the future", testSecret, "msg_1", ts, good, body, now.Add(-6 * time.Minute), ErrInvalidSignature},
		{"timestamp just inside the window", testSecret, "msg_1", ts, good, body, now.Add(4 * time.Minute), nil},
		{"unversioned signature ignored", testSecret, "msg_1", ts, "v2," + good[3:], body, now, ErrInvalidSignature},
		{"garbled base64 signature ignored", testSecret, "msg_1", ts, "v1,@@@not-base64@@@", body, now, ErrInvalidSignature},
		{"empty signature header", testSecret, "msg_1", ts, "", body, now, ErrInvalidSignature},
		{"empty message id", testSecret, "", ts, good, body, now, ErrInvalidSignature},
		{"non-numeric timestamp", testSecret, "msg_1", "yesterday", good, body, now, ErrInvalidSignature},
		{"empty timestamp", testSecret, "msg_1", "", good, body, now, ErrInvalidSignature},
		{"secret is not base64", "whsec_!!!", "msg_1", ts, good, body, now, ErrBadSecret},
		{"empty secret", "", "msg_1", ts, good, body, now, ErrBadSecret},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifySignature(tc.secret, tc.msgID, tc.timestamp, tc.sigs, tc.body, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("VerifySignature() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestSignatureHeader_RejectsBadSecret(t *testing.T) {
	if _, err := SignatureHeader("whsec_!!!", "msg_1", "1", nil); !errors.Is(err, ErrBadSecret) {
		t.Fatalf("SignatureHeader(bad secret) error = %v, want ErrBadSecret", err)
	}
}
