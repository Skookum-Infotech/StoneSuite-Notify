package emailevents

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Resend signs webhooks with Svix. The signed content is
// "{svix-id}.{svix-timestamp}.{raw body}"; the key is the base64 payload of the
// "whsec_…" secret; the svix-signature header is a space-separated list of
// "v1,<base64 HMAC-SHA256>" entries (more than one while a secret is rotating).
const (
	svixSecretPrefix       = "whsec_"
	svixSignatureVersion   = "v1"
	svixTimestampTolerance = 5 * time.Minute
)

var (
	// ErrInvalidSignature means the request failed authentication: a missing or
	// malformed header, a timestamp outside the replay window, or no matching signature.
	ErrInvalidSignature = errors.New("emailevents: invalid webhook signature")
	// ErrBadSecret means the configured signing secret is not a valid
	// "whsec_<base64>" value — a server misconfiguration, not a bad request.
	ErrBadSecret = errors.New("emailevents: webhook secret is not a valid whsec_ secret")
)

func svixKey(secret string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, svixSecretPrefix))
	if err != nil || len(key) == 0 {
		return nil, ErrBadSecret
	}
	return key, nil
}

func svixMAC(key []byte, msgID, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID + "." + timestamp + "."))
	mac.Write(body)
	return mac.Sum(nil)
}

// SignatureHeader returns the svix-signature header value ("v1,<base64>") for a
// body. Real webhooks are signed by Resend; this exists for tests and for
// scripts/signwebhook, which lets a developer exercise the route locally.
func SignatureHeader(secret, msgID, timestamp string, body []byte) (string, error) {
	key, err := svixKey(secret)
	if err != nil {
		return "", err
	}
	return svixSignatureVersion + "," + base64.StdEncoding.EncodeToString(svixMAC(key, msgID, timestamp, body)), nil
}

// VerifySignature authenticates a Svix-signed webhook. body must be the raw
// request bytes — re-marshalled JSON would not match. It accepts the request if
// ANY entry of signatures matches (constant-time compare) and the timestamp is
// within five minutes of now in either direction, which bounds replay.
func VerifySignature(secret, msgID, timestamp, signatures string, body []byte, now time.Time) error {
	key, err := svixKey(secret)
	if err != nil {
		return err
	}
	if msgID == "" {
		return ErrInvalidSignature
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	skew := now.Sub(time.Unix(seconds, 0))
	if skew < 0 {
		skew = -skew
	}
	if skew > svixTimestampTolerance {
		return ErrInvalidSignature
	}

	want := svixMAC(key, msgID, timestamp, body)
	for _, candidate := range strings.Fields(signatures) {
		version, encoded, ok := strings.Cut(candidate, ",")
		if !ok || version != svixSignatureVersion {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrInvalidSignature
}
