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
	// A received file goes to <path>/{ref}/attachments/{id}.
	backendInboundPath = "/api/internal/email/inbound"
	// timestampHeader and signatureHeader are the names StoneSuite-Backend's
	// middleware.VerifyInternalSignature reads (shared with its cron-tick route).
	timestampHeader = "X-Automation-Timestamp"
	signatureHeader = "X-Automation-Signature"

	// forwardTimeout allows for the backend's scale-to-zero cold start.
	forwardTimeout = 20 * time.Second
	// uploadTimeout covers a file of up to MaxFileBytes plus that cold start.
	uploadTimeout = 90 * time.Second

	// responseBodyLimit bounds what is read back from the backend: the answer to a
	// forwarded mail lists the ids of its pending files.
	responseBodyLimit = 64 << 10
)

// HTTPForwarder is the Forwarder (and FileUploader) that POSTs a mail to
// StoneSuite-Backend, and PUTs each of its files, signed with the secret both
// services already share.
type HTTPForwarder struct {
	BackendURL string
	Secret     string
	HTTP       *http.Client
	// Now is injectable so tests can pin the signed timestamp. Defaults to time.Now.
	Now func() time.Time
}

// Forward implements Forwarder.
func (f HTTPForwarder) Forward(ctx context.Context, m Mail) (ForwardResult, error) {
	endpoint, err := url.JoinPath(f.BackendURL, backendInboundPath)
	if err != nil {
		return ForwardResult{}, fmt.Errorf("build backend inbound url: %w", err)
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return ForwardResult{}, fmt.Errorf("encode forwarded mail: %w", err)
	}
	body, err := f.do(ctx, http.MethodPost, endpoint, "application/json", payload, forwardTimeout, "backend inbound")
	if err != nil {
		return ForwardResult{}, err
	}
	// An older backend answers with just {"success":true}, and a body that is not
	// the expected JSON means the same thing: no files to copy. The mail itself was
	// accepted, which is what matters here.
	var res ForwardResult
	_ = json.Unmarshal(body, &res)
	return res, nil
}

// UploadAttachment implements FileUploader: the file's bytes as the body of
// PUT <backend>/api/internal/email/inbound/{ref}/attachments/{attachmentID}.
func (f HTTPForwarder) UploadAttachment(ctx context.Context, ref, attachmentID, contentType string, data []byte) error {
	endpoint, err := url.JoinPath(f.BackendURL, backendInboundPath, url.PathEscape(ref), "attachments", url.PathEscape(attachmentID))
	if err != nil {
		return fmt.Errorf("build backend attachment url: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err = f.do(ctx, http.MethodPut, endpoint, contentType, data, uploadTimeout, "backend inbound attachment")
	return err
}

// do sends one signed request to the backend and returns the body of a 2xx
// answer. Any other answer is an error, marked retryable when a later attempt
// could succeed (see statusError).
func (f HTTPForwarder) do(
	ctx context.Context, method, endpoint, contentType string, payload []byte, timeout time.Duration, what string,
) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse %s url: %w", what, err)
	}
	now := f.Now
	if now == nil {
		now = time.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", what, err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(timestampHeader, ts)
	req.Header.Set(signatureHeader, sign(f.Secret, method, u.Path, ts, payload))

	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("%s: %w", what, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, responseBodyLimit))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	return nil, statusError(what, resp.StatusCode, body)
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
