// Package channels delivers a persisted notification over side channels
// beyond the in-app feed: email and web push. Both senders are
// fire-and-forget from the caller's perspective — a delivery failure here
// must never fail the notification-create request, since the in-app row
// already exists and is the source of truth.
package channels

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"regexp"
	"strings"
	"time"

	"stonesuite-notify/config"
)

const emailTimeout = 10 * time.Second

// resendEndpoint is Resend's send-email API. A package var, not a const, so
// tests can point it at an httptest server.
var resendEndpoint = "https://api.resend.com/emails"

// EmailAttachment is the single file this channel can attach to a
// notification email — currently only ever the PDF from a "document sent"
// event. Defined here (not imported from notifications) so this package
// stays dependency-free of the domain layer, matching its existing
// config-only import list.
type EmailAttachment struct {
	FileName    string
	ContentType string
	Content     []byte
}

// EmailOptions are the per-message header overrides a caller can request on
// top of the service-wide defaults: a Reply-To that routes a customer's reply
// back to a specific record, a human sender name, and the threading headers
// that make Gmail/Outlook keep a reply in the same conversation. The zero value
// changes nothing.
type EmailOptions struct {
	ReplyTo    string
	FromName   string
	InReplyTo  string
	References string
}

// replyTo returns the Reply-To to use: the per-message one when set, else the
// service-wide EMAIL_REPLY_TO.
func (o EmailOptions) replyTo(cfg config.Config) string {
	if o.ReplyTo != "" {
		return sanitizeHeader(o.ReplyTo)
	}
	return cfg.EmailReplyTo
}

// from returns the From header value: the configured sender address, shown
// under FromName when one is given. mail.Address quotes and RFC 2047-encodes
// the name, so a name with commas, quotes or non-ASCII text stays valid.
func (o EmailOptions) from(cfg config.Config) string {
	name := sanitizeHeader(o.FromName)
	if name == "" {
		return cfg.EmailFrom
	}
	parsed, err := mail.ParseAddress(cfg.EmailFrom)
	if err != nil {
		return cfg.EmailFrom
	}
	return (&mail.Address{Name: name, Address: parsed.Address}).String()
}

// threadHeaders returns the In-Reply-To / References pair for a reply, or nil
// when neither is set.
func (o EmailOptions) threadHeaders() map[string]string {
	h := map[string]string{}
	if v := sanitizeHeader(o.InReplyTo); v != "" {
		h["In-Reply-To"] = v
	}
	if v := sanitizeHeader(o.References); v != "" {
		h["References"] = v
	}
	if len(h) == 0 {
		return nil
	}
	return h
}

// sanitizeHeader drops CR/LF so a caller-supplied value can never inject an
// extra header line into the message.
func sanitizeHeader(v string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(v))
}

// SendNotificationEmail delivers a notification over email, choosing a
// provider the same way StoneSuite-Backend's services/email.go does:
// Resend if configured, else SMTP. A missing provider or a missing
// EMAIL_FROM returns an error (the delivery then retries and goes
// terminal with a reason) rather than a silent no-op, since a delivery
// row only reaches this function when email was actually requested.
// attachment is optional (nil for the overwhelming majority of
// notifications). When emailBodyHTML is non-empty, it is used verbatim as
// the message body instead of the generic <h2>title</h2><p>body</p>
// template — used by callers (e.g. a document-send customer email) that
// need their own branding. Errors are returned for the worker to log and
// retry on, never surfaced to the notification-create response.
// It returns the provider's id for the sent email (Resend only; "" for SMTP),
// which the worker stores so the provider's delivery webhooks can be matched
// to this delivery.
func SendNotificationEmail(cfg config.Config, to, title, body, link, emailBodyHTML string, attachment *EmailAttachment, opts EmailOptions) (string, error) {
	if to == "" {
		return "", fmt.Errorf("email channel: recipient address is empty")
	}
	// A configured provider with no EMAIL_FROM cannot send: Resend rejects
	// the request with HTTP 422 (invalid "from"), and SMTP has no envelope
	// sender. Fail here with a message that names the missing variable
	// instead of relaying an opaque provider error — EMAIL_FROM is a Fly
	// secret, easy to drop in a secrets reset and (historically) documented
	// only under SMTP.
	if cfg.EmailConfigured() && cfg.EmailFrom == "" {
		return "", fmt.Errorf("email channel: EMAIL_FROM is not set (required as the sender address for Resend and SMTP alike)")
	}

	htmlBody := emailBodyHTML
	if htmlBody == "" {
		htmlBody = renderEmailHTML(title, body, link)
	}
	// A multipart message with a text/plain alternative scores markedly
	// better with spam filters than an HTML-only body. The templates this
	// service receives are well-formed and inline-styled (see
	// StoneSuite-Backend services.WrapEmailHTML), so a small derivation is
	// enough — no need for the caller to send a second body.
	textBody := htmlToPlainText(htmlBody)

	switch {
	case cfg.ResendAPIKey != "":
		return sendViaResend(cfg, to, title, htmlBody, textBody, attachment, opts)
	case cfg.SMTPHost != "":
		return "", sendViaSMTP(cfg, to, title, htmlBody, textBody, attachment, opts)
	default:
		// A delivery row only reaches this worker because Create decided
		// email was wanted and enabled — so "no provider" here is a
		// misconfiguration, not a no-op. Return an error so the delivery
		// goes retrying -> failed with a clear reason, never marked sent.
		return "", fmt.Errorf("email channel: no provider configured (set RESEND_API_KEY or SMTP_HOST, plus EMAIL_FROM)")
	}
}

func renderEmailHTML(title, body, link string) string {
	linkHTML := ""
	if link != "" {
		linkHTML = fmt.Sprintf(`<p><a href="%s">View in StoneSuite</a></p>`, link)
	}
	return fmt.Sprintf(`<div><h2>%s</h2><p>%s</p>%s</div>`, title, body, linkHTML)
}

var (
	reMailHead   = regexp.MustCompile(`(?is)<head\b.*?</head>`)
	reMailScript = regexp.MustCompile(`(?is)<(script|style)\b.*?</(script|style)>`)
	reMailHidden = regexp.MustCompile(`(?is)<div[^>]*display:\s*none[^>]*>.*?</div>`)
	reMailAnchor = regexp.MustCompile(`(?is)<a\b[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	reMailBlock  = regexp.MustCompile(`(?is)</(p|div|tr|h1|h2|h3|li)>|<br\s*/?>`)
	reMailTag    = regexp.MustCompile(`(?s)<[^>]+>`)
	reMailBlanks = regexp.MustCompile(`\n{3,}`)
)

// htmlToPlainText derives a text/plain alternative from a transactional
// email's HTML body. It is deliberately minimal — it assumes the well-formed,
// inline-styled markup this service receives (see StoneSuite-Backend
// services.WrapEmailHTML and controllers document/welcome emails), not
// arbitrary HTML. Anchors become "label: url" so the action link survives in
// the text part; the hidden preheader and document head are dropped.
func htmlToPlainText(h string) string {
	s := reMailHead.ReplaceAllString(h, "")
	s = reMailScript.ReplaceAllString(s, "")
	s = reMailHidden.ReplaceAllString(s, "")
	s = reMailAnchor.ReplaceAllString(s, "$2: $1")
	s = reMailBlock.ReplaceAllString(s, "\n")
	s = reMailTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(strings.Join(strings.Fields(lines[i]), " "))
	}
	s = reMailBlanks.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}

type resendAttachment struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type resendRequest struct {
	From        string             `json:"from"`
	To          string             `json:"to"`
	ReplyTo     string             `json:"reply_to,omitempty"`
	Headers     map[string]string  `json:"headers,omitempty"`
	Subject     string             `json:"subject"`
	HTML        string             `json:"html"`
	Text        string             `json:"text,omitempty"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
}

// resendResponse is the success body of Resend's POST /emails.
type resendResponse struct {
	ID string `json:"id"`
}

// sendViaResend sends through Resend and returns Resend's id for the accepted
// email ("" if the response carried none).
func sendViaResend(cfg config.Config, to, subject, htmlBody, textBody string, attachment *EmailAttachment, opts EmailOptions) (string, error) {
	req := resendRequest{
		From:    opts.from(cfg),
		To:      to,
		ReplyTo: opts.replyTo(cfg),
		Headers: opts.threadHeaders(),
		Subject: subject,
		HTML:    htmlBody,
		Text:    textBody,
	}
	if attachment != nil {
		req.Attachments = []resendAttachment{{
			Filename: attachment.FileName,
			Content:  base64.StdEncoding.EncodeToString(attachment.Content),
		}}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("resend: encode request: %w", err)
	}

	client := &http.Client{Timeout: emailTimeout}
	httpReq, err := http.NewRequest(http.MethodPost, resendEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("resend: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.ResendAPIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("resend: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Include Resend's response body — it carries the actual reason
		// (e.g. an unverified sending domain, or an invalid "from"), which a
		// bare status code hides. This lands in notification_deliveries.last_error
		// and the worker log, so a recurring 422 is self-diagnosing.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("resend: unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	// The email was accepted. Its id is what Resend's later delivery webhooks
	// refer to; failing to read it must not fail a send that already happened.
	var accepted resendResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&accepted); err != nil {
		return "", nil
	}
	return accepted.ID, nil
}

func sendViaSMTP(cfg config.Config, to, subject, htmlBody, textBody string, attachment *EmailAttachment, opts EmailOptions) error {
	addr := cfg.SMTPHost + ":" + cfg.SMTPPort
	msg := buildSMTPMessage(to, opts.from(cfg), opts.replyTo(cfg), subject, htmlBody, textBody, attachment, opts)

	var auth smtp.Auth
	if cfg.SMTPUsername != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, cfg.SMTPHost)
	}

	if err := smtp.SendMail(addr, auth, cfg.EmailFrom, []string{to}, msg); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	return nil
}

// mimeBody returns the message's body entity and its Content-Type: a lone
// text/html part, or a multipart/alternative pairing text/plain with
// text/html when altText is set. buildSMTPMessage embeds this directly, or as
// the first part of a multipart/mixed when there's an attachment.
func mimeBody(htmlBody, altText string) (contentType string, body []byte) {
	if altText == "" {
		return "text/html; charset=UTF-8", []byte(htmlBody)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, part := range []struct{ ct, content string }{
		{"text/plain; charset=UTF-8", altText},
		{"text/html; charset=UTF-8", htmlBody},
	} {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", part.ct)
		hdr.Set("Content-Transfer-Encoding", "8bit")
		if pw, err := mw.CreatePart(hdr); err == nil {
			_, _ = pw.Write([]byte(part.content))
		}
	}
	_ = mw.Close()
	return "multipart/alternative; boundary=" + mw.Boundary(), buf.Bytes()
}

// buildSMTPMessage assembles the raw RFC 5322 message. The body is a
// text/html part, or a multipart/alternative (text/plain then text/html) when
// a plain-text alternative is supplied; an attachment wraps that body in an
// outer multipart/mixed. Mirrors the html/text/reply_to shape Resend builds,
// so both providers deliver the same message.
func buildSMTPMessage(to, from, replyTo, subject, htmlBody, textBody string, attachment *EmailAttachment, opts EmailOptions) []byte {
	bodyCT, bodyBytes := mimeBody(htmlBody, textBody)

	var buf bytes.Buffer
	buf.WriteString("To: " + to + "\r\n")
	// The From header is only written when a sender name was requested; the
	// envelope sender (smtp.SendMail's from argument) is what the server
	// authenticates, and an unnamed message keeps the provider's default.
	if opts.FromName != "" {
		buf.WriteString("From: " + from + "\r\n")
	}
	if replyTo != "" {
		buf.WriteString("Reply-To: " + replyTo + "\r\n")
	}
	threading := opts.threadHeaders()
	for _, name := range []string{"In-Reply-To", "References"} {
		if v, ok := threading[name]; ok {
			buf.WriteString(name + ": " + v + "\r\n")
		}
	}
	buf.WriteString("Subject: " + subject + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")

	if attachment == nil {
		buf.WriteString("Content-Type: " + bodyCT + "\r\n\r\n")
		buf.Write(bodyBytes)
		buf.WriteString("\r\n")
		return buf.Bytes()
	}

	mw := multipart.NewWriter(&buf)
	buf.WriteString("Content-Type: multipart/mixed; boundary=" + mw.Boundary() + "\r\n\r\n")

	bodyHdr := textproto.MIMEHeader{}
	bodyHdr.Set("Content-Type", bodyCT)
	if pw, err := mw.CreatePart(bodyHdr); err == nil {
		_, _ = pw.Write(bodyBytes)
	}

	ct := attachment.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	attHdr := textproto.MIMEHeader{}
	attHdr.Set("Content-Type", ct)
	attHdr.Set("Content-Transfer-Encoding", "base64")
	attHdr.Set("Content-Disposition", `attachment; filename="`+attachment.FileName+`"`)
	if pw, err := mw.CreatePart(attHdr); err == nil {
		b64 := base64.StdEncoding.EncodeToString(attachment.Content)
		for i := 0; i < len(b64); i += 76 {
			end := i + 76
			if end > len(b64) {
				end = len(b64)
			}
			pw.Write([]byte(b64[i:end] + "\r\n"))
		}
	}
	_ = mw.Close()
	return buf.Bytes()
}
