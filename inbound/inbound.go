// Package inbound handles mail Resend receives on the reply domain: a customer
// answering an email StoneSuite sent about a record. It fetches the mail's
// content from Resend and forwards it to StoneSuite-Backend, which owns the
// record data and files it under the record the reply address names. This
// service never decides which record a mail belongs to; it only keeps Resend's
// credentials and API on this side of the system.
package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// MaxFileBytes is the largest received file that is copied to the backend. It
// matches the backend's own limit (recordemail.InboundMaxFileBytes): a bigger file
// is recorded there as too large and never asked for.
const MaxFileBytes = 20 << 20

const (
	// EventEmailReceived is the Resend webhook event for an inbound mail.
	EventEmailReceived = "email.received"

	// maxBodyBytes bounds each of the text and html parts forwarded. The backend
	// keeps only the first 20k characters of the reply itself, which sits at the
	// top of the mail, so a long quoted history behind it is dead weight.
	maxBodyBytes = 256 << 10
)

// Event is the part of Resend's email.received webhook this package reads. The
// webhook carries metadata only; the body comes from a Fetcher.
type Event struct {
	Type string    `json:"type"`
	Data EventData `json:"data"`
}

// EventData is the payload of an email.received event.
type EventData struct {
	EmailID     string       `json:"email_id"`
	From        string       `json:"from"`
	To          []string     `json:"to"`
	Cc          []string     `json:"cc"`
	Bcc         []string     `json:"bcc"`
	MessageID   string       `json:"message_id"`
	Subject     string       `json:"subject"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment is the webhook's metadata of one attached file: just an id and a
// name. The size, type and whether it is inline come from the attachments API
// (see ReceivedAttachment).
type Attachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
}

// ParseEvent decodes a webhook body.
func ParseEvent(body []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, fmt.Errorf("decode inbound webhook: %w", err)
	}
	return ev, nil
}

// Recipients returns every address the mail was sent to (to, cc, bcc). A record's
// reply address can be any of them: a CC'd or BCC'd copy still arrives on our domain.
func (e Event) Recipients() []string {
	out := make([]string, 0, len(e.Data.To)+len(e.Data.Cc)+len(e.Data.Bcc))
	out = append(out, e.Data.To...)
	out = append(out, e.Data.Cc...)
	return append(out, e.Data.Bcc...)
}

// Received is a full inbound mail as returned by Resend's Receiving API.
type Received struct {
	ID        string         `json:"id"`
	From      string         `json:"from"`
	To        []string       `json:"to"`
	Cc        []string       `json:"cc"`
	Subject   string         `json:"subject"`
	Text      string         `json:"text"`
	HTML      string         `json:"html"`
	MessageID string         `json:"message_id"`
	Headers   map[string]any `json:"headers"`
}

// Header returns a header's value, matched case-insensitively ("" if absent).
func (r Received) Header(name string) string {
	for k, v := range r.Headers {
		if strings.EqualFold(k, name) {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// Mail is what is forwarded to the backend: the fetched mail with the webhook's
// metadata filled in wherever the fetched copy lacks it. Quoted-history stripping
// and size limits on the stored message are the backend's job, not this one's.
type Mail struct {
	EmailID         string   `json:"emailId"`
	From            string   `json:"from"`
	To              []string `json:"to"`
	CC              []string `json:"cc"`
	BCC             []string `json:"bcc"`
	Subject         string   `json:"subject"`
	MessageID       string   `json:"messageId"`
	InReplyTo       string   `json:"inReplyTo"`
	References      string   `json:"references"`
	Text            string   `json:"text"`
	HTML            string   `json:"html"`
	AttachmentCount int      `json:"attachmentCount"`
	// Attachments describes each attached file, so the backend can record them and
	// say which ones it wants copied. Empty when files cannot be copied (no job
	// store), which leaves only the count above, as before.
	Attachments []MailAttachment `json:"attachments"`
}

// MailAttachment is one file's metadata in the forwarded mail.
type MailAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Inline      bool   `json:"inline"`
}

// ReceivedAttachment is one file of a received mail as Resend's attachments API
// lists it. DownloadURL is a time-limited link (about an hour), so it is fetched
// fresh for every attempt to copy the file, never stored.
type ReceivedAttachment struct {
	ID                 string `json:"id"`
	Filename           string `json:"filename"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
	ContentDisposition string `json:"content_disposition"`
	ContentID          string `json:"content_id"`
	DownloadURL        string `json:"download_url"`
	ExpiresAt          string `json:"expires_at"`
}

// IsInline reports whether the file is shown inside the mail body (a pasted
// picture, a signature logo) rather than attached to it.
func (a ReceivedAttachment) IsInline() bool { return strings.EqualFold(a.ContentDisposition, "inline") }

// BuildMail merges the webhook event, the fetched mail and the listed files into
// the payload the backend receives. The fetched copy wins; the webhook fills any
// gap. files is nil when the files were not listed.
func BuildMail(ev Event, rec Received, files []ReceivedAttachment) Mail {
	var attachments []MailAttachment
	for _, f := range files {
		attachments = append(attachments, MailAttachment{
			ID: f.ID, Filename: f.Filename, ContentType: f.ContentType, Size: f.Size, Inline: f.IsInline(),
		})
	}
	return Mail{
		EmailID:         ev.Data.EmailID,
		From:            firstNonEmpty(rec.From, ev.Data.From),
		To:              firstNonEmptySlice(rec.To, ev.Data.To),
		CC:              firstNonEmptySlice(rec.Cc, ev.Data.Cc),
		BCC:             ev.Data.Bcc,
		Subject:         firstNonEmpty(rec.Subject, ev.Data.Subject),
		MessageID:       firstNonEmpty(rec.MessageID, ev.Data.MessageID),
		InReplyTo:       rec.Header("in-reply-to"),
		References:      rec.Header("references"),
		Text:            truncateBytes(rec.Text, maxBodyBytes),
		HTML:            truncateBytes(rec.HTML, maxBodyBytes),
		AttachmentCount: len(ev.Data.Attachments),
		Attachments:     attachments,
	}
}

// AddressedToDomain reports whether any of addrs is an address on domain. Each
// entry may be a bare address or "Name <addr>"; unparseable entries are skipped.
func AddressedToDomain(addrs []string, domain string) bool {
	for _, raw := range addrs {
		parsed, err := mail.ParseAddress(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		at := strings.LastIndex(parsed.Address, "@")
		if at >= 0 && strings.EqualFold(parsed.Address[at+1:], domain) {
			return true
		}
	}
	return false
}

// Fetcher loads a received mail's full content by its provider id.
type Fetcher interface {
	Fetch(ctx context.Context, emailID string) (Received, error)
}

// ForwardResult is what the backend answers to a forwarded mail: the files it
// still wants copied (Pending, the provider's attachment ids) and the UploadRef to
// send them under. Both are empty when there is nothing to copy, and always are
// for a backend that does not copy files.
type ForwardResult struct {
	UploadRef string   `json:"uploadRef"`
	Pending   []string `json:"pending"`
}

// Forwarder hands a received mail to the backend.
type Forwarder interface {
	Forward(ctx context.Context, m Mail) (ForwardResult, error)
}

// AttachmentLister lists a received mail's files, with fresh download links.
type AttachmentLister interface {
	ListAttachments(ctx context.Context, emailID string) ([]ReceivedAttachment, error)
}

// FileDownloader fetches one received file's bytes from its download link,
// refusing one larger than maxBytes (ErrFileTooLarge).
type FileDownloader interface {
	Download(ctx context.Context, downloadURL string, maxBytes int64) ([]byte, error)
}

// FileUploader hands one received file to the backend, under the UploadRef its
// answer to the forwarded mail named.
type FileUploader interface {
	UploadAttachment(ctx context.Context, ref, attachmentID, contentType string, data []byte) error
}

// Outcome says what Process did with an event.
type Outcome string

const (
	// OutcomeIgnored means the mail was not ours to handle.
	OutcomeIgnored Outcome = "ignored"
	// OutcomeForwarded means the backend accepted the mail.
	OutcomeForwarded Outcome = "forwarded"
)

// Processor turns an email.received event into a forwarded mail, and queues the
// copying of the files that came with it.
type Processor struct {
	// Domain is the reply domain (REPLY_EMAIL_DOMAIN): mail not addressed to it
	// is ignored without being fetched.
	Domain    string
	Fetcher   Fetcher
	Forwarder Forwarder
	// Lister and Jobs enable copying files: both or neither. Without them a mail
	// is forwarded with only its attachment count.
	Lister AttachmentLister
	Jobs   JobStore
}

// Process handles one event. A mail for another domain is ignored. Any error is
// either a *RetryableError (a later attempt could succeed, so the webhook should
// be answered with a failure and Resend will retry) or permanent.
//
// The files are not copied here: the webhook has only seconds to answer. The
// backend records them and names the ones it wants; those are queued as jobs for
// the worker, so a retried webhook only queues what is still wanted.
func (p Processor) Process(ctx context.Context, ev Event) (Outcome, error) {
	if ev.Data.EmailID == "" || !AddressedToDomain(ev.Recipients(), p.Domain) {
		return OutcomeIgnored, nil
	}
	rec, err := p.Fetcher.Fetch(ctx, ev.Data.EmailID)
	if err != nil {
		return "", fmt.Errorf("fetch received email: %w", err)
	}

	var files []ReceivedAttachment
	if len(ev.Data.Attachments) > 0 && p.Lister != nil && p.Jobs != nil {
		if files, err = p.Lister.ListAttachments(ctx, ev.Data.EmailID); err != nil {
			return "", fmt.Errorf("list received attachments: %w", err)
		}
	}
	res, err := p.Forwarder.Forward(ctx, BuildMail(ev, rec, files))
	if err != nil {
		return "", fmt.Errorf("forward received email: %w", err)
	}
	if p.Jobs != nil && res.UploadRef != "" && len(res.Pending) > 0 {
		if err := p.Jobs.Enqueue(ctx, ev.Data.EmailID, res.UploadRef, res.Pending); err != nil {
			// The mail is filed already; answering with a failure makes Resend retry,
			// which forwards it again (idempotent) and queues the files again.
			return "", &RetryableError{Err: fmt.Errorf("queue received attachments: %w", err)}
		}
	}
	return OutcomeForwarded, nil
}

// RetryableError marks a failure that a later attempt could fix: the network, a
// 5xx, or a misconfiguration an operator can correct while Resend is still
// retrying.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// IsRetryable reports whether err is, or wraps, a *RetryableError.
func IsRetryable(err error) bool {
	var r *RetryableError
	return errors.As(err, &r)
}

// retryableStatus reports whether an HTTP status from Resend or the backend is
// worth another attempt: server errors and rate limits, plus the few 4xx that
// mean "set up wrong, fixable" (bad key or secret, route not live yet).
func retryableStatus(status int) bool {
	switch status {
	case 401, 403, 404, 408, 429:
		return true
	}
	return status >= 500
}

// statusError wraps a non-success response as an error, marked retryable when a
// later attempt could succeed.
func statusError(what string, status int, body []byte) error {
	err := fmt.Errorf("%s returned %d: %s", what, status, strings.TrimSpace(string(body)))
	if retryableStatus(status) {
		return &RetryableError{Err: err}
	}
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonEmptySlice(values ...[]string) []string {
	for _, v := range values {
		if len(v) > 0 {
			return v
		}
	}
	return nil
}

// truncateBytes cuts s to at most max bytes without splitting a UTF-8 character.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
