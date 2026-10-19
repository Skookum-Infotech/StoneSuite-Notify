package inbound

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// backendInboundPath is the backend's machine-only route for a received reply.
	backendInboundPath = "/api/internal/email/inbound"
	// timestampHeader and signatureHeader are the names StoneSuite-Backend's
	// middleware.VerifyInternalSignature reads (shared with its cron-tick route).
	timestampHeader = "X-Automation-Timestamp"
	signatureHeader = "X-Automation-Signature"

	// forwardTimeout allows for the backend's scale-to-zero cold start.
	forwardTimeout = 20 * time.Second
)

// HTTPForwarder is the Forwarder that POSTs a mail to StoneSuite-Backend, signed
// with the secret both services already share.
type HTTPForwarder struct {
	BackendURL string
	Secret     string
	HTTP       *http.Client
	// Now is injectable so tests can pin the signed timestamp. Defaults to time.Now.
	Now func() time.Time
}

// Forward implements Forwarder.
func (f HTTPForwarder) Forward(ctx context.Context, m Mail) error {
	endpoint, err := url.JoinPath(f.BackendURL, backendInboundPath)
	if err != nil {
		return fmt.Errorf("build backend inbound url: %w", err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parse backend inbound url: %w", err)
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode forwarded mail: %w", err)
	}

	now := f.Now
	if now == nil {
		now = time.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build backend inbound request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(signatureHeader, sign(f.Secret, http.MethodPost, u.Path, ts, payload))

	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: forwardTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &RetryableError{Err: fmt.Errorf("backend inbound: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, errorBodyLimit))
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	return statusError("backend inbound", resp.StatusCode, body)
}

// sign computes hex(HMAC-SHA256(secret, method + " " + path + "\n" + ts + "\n" + body)),
// the signature StoneSuite-Backend's VerifyInternalSignature expects. The path is
// part of the signed text, so a capture for one internal route can't be replayed
// on another.
func sign(secret, method, path, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + " " + path + "\n"))
	mac.Write([]byte(ts))
	mac.Write([]byte("\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
