package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	resendReceivingURL = "https://api.resend.com/emails/receiving/"
	fetchTimeout       = 10 * time.Second
	fetchBodyLimit     = 8 << 20
	errorBodyLimit     = 4 << 10

	// downloadTimeout bounds one file's download; a file is at most MaxFileBytes.
	downloadTimeout = 45 * time.Second
	// downloadHostSuffix is where Resend serves received files from
	// (inbound-cdn.resend.com). The link comes from Resend's own API answer, not
	// from the sender, but a link to anywhere else is refused all the same.
	downloadHostSuffix = ".resend.com"
)

// ErrFileTooLarge means a received file is bigger than the limit asked for. It is
// permanent: waiting does not make the file smaller.
var ErrFileTooLarge = errors.New("received attachment is larger than the limit")

// ResendFetcher is the Fetcher backed by Resend's Receiving API. It also lists
// a mail's files and downloads them (AttachmentLister, FileDownloader).
type ResendFetcher struct {
	APIKey string
	// BaseURL overrides the API origin path (tests). Defaults to Resend's.
	BaseURL string
	HTTP    *http.Client
	// CheckDownloadURL decides whether a file may be downloaded from a link. It
	// defaults to https on a *.resend.com host; tests point it at a local server.
	CheckDownloadURL func(*url.URL) error
}

func (c ResendFetcher) base() string {
	if c.BaseURL == "" {
		return resendReceivingURL
	}
	return c.BaseURL
}

func (c ResendFetcher) client(timeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: timeout}
}

// get calls Resend's receiving API at path under the base, with the bearer key,
// and returns the body of a 200. what names the call in errors.
func (c ResendFetcher) get(ctx context.Context, path, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build %s request: %w", what, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.client(fetchTimeout).Do(req)
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("%s: %w", what, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return nil, statusError(what, resp.StatusCode, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchBodyLimit))
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("read %s: %w", what, err)}
	}
	return body, nil
}

// Fetch implements Fetcher.
func (c ResendFetcher) Fetch(ctx context.Context, emailID string) (Received, error) {
	body, err := c.get(ctx, url.PathEscape(emailID), "resend receiving "+emailID)
	if err != nil {
		return Received{}, err
	}
	var out Received
	if err := json.Unmarshal(body, &out); err != nil {
		return Received{}, fmt.Errorf("decode received email %s: %w", emailID, err)
	}
	return out, nil
}

// ListAttachments implements AttachmentLister: every file of the received mail,
// each with a download link valid for about an hour.
func (c ResendFetcher) ListAttachments(ctx context.Context, emailID string) ([]ReceivedAttachment, error) {
	what := "resend receiving attachments " + emailID
	body, err := c.get(ctx, url.PathEscape(emailID)+"/attachments", what)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []ReceivedAttachment `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode received attachments %s: %w", emailID, err)
	}
	return out.Data, nil
}

// defaultDownloadURLCheck allows https links on Resend's own hosts only.
func defaultDownloadURLCheck(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("download link is not https (%s)", u.Scheme)
	}
	if !strings.HasSuffix(strings.ToLower(u.Hostname()), downloadHostSuffix) {
		return fmt.Errorf("download link host %q is not a %s host", u.Hostname(), downloadHostSuffix)
	}
	return nil
}

// Download implements FileDownloader. The link needs no credentials. A file
// larger than maxBytes is refused with ErrFileTooLarge, before it is read when
// its size is declared; the buffer is sized once for a declared length instead of
// being grown, since a file is held whole on a 256 MB machine.
func (c ResendFetcher) Download(ctx context.Context, downloadURL string, maxBytes int64) ([]byte, error) {
	u, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("parse download link: %w", err)
	}
	check := c.CheckDownloadURL
	if check == nil {
		check = defaultDownloadURLCheck
	}
	if err := check(u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := c.client(downloadTimeout).Do(req)
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("download received attachment: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return nil, statusError("download received attachment", resp.StatusCode, body)
	}
	if resp.ContentLength > maxBytes {
		return nil, ErrFileTooLarge
	}
	if n := resp.ContentLength; n > 0 {
		buf := make([]byte, n)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			return nil, &RetryableError{Err: fmt.Errorf("read received attachment: %w", err)}
		}
		return buf, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, &RetryableError{Err: fmt.Errorf("read received attachment: %w", err)}
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrFileTooLarge
	}
	return data, nil
}
