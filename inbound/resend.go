package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const (
	resendReceivingURL = "https://api.resend.com/emails/receiving/"
	fetchTimeout       = 10 * time.Second
	fetchBodyLimit     = 8 << 20
	errorBodyLimit     = 4 << 10
)

// ResendFetcher is the Fetcher backed by Resend's Receiving API.
type ResendFetcher struct {
	APIKey string
	// BaseURL overrides the API origin path (tests). Defaults to Resend's.
	BaseURL string
	HTTP    *http.Client
}

// Fetch implements Fetcher.
func (c ResendFetcher) Fetch(ctx context.Context, emailID string) (Received, error) {
	base := c.BaseURL
	if base == "" {
		base = resendReceivingURL
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+url.PathEscape(emailID), nil)
	if err != nil {
		return Received{}, fmt.Errorf("build resend receiving request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := client.Do(req)
	if err != nil {
		return Received{}, &RetryableError{Err: fmt.Errorf("resend receiving %s: %w", emailID, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return Received{}, statusError("resend receiving "+emailID, resp.StatusCode, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchBodyLimit))
	if err != nil {
		return Received{}, &RetryableError{Err: fmt.Errorf("read received email %s: %w", emailID, err)}
	}
	var out Received
	if err := json.Unmarshal(body, &out); err != nil {
		return Received{}, fmt.Errorf("decode received email %s: %w", emailID, err)
	}
	return out, nil
}
