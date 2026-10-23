package channels

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"stonesuite-notify/config"
)

func TestSendNotificationEmail_NoProviderConfigured_ReturnsError(t *testing.T) {
	// A delivery row only reaches this function because Create queued an
	// email delivery, so "no provider" is a misconfiguration: it must fail
	// (delivery -> retrying -> failed with a reason), never return nil,
	// which would mark the delivery sent.
	_, err := SendNotificationEmail(config.Config{}, "user@example.com", "title", "body", "", "", nil, EmailOptions{})
	if err == nil {
		t.Fatal("expected an error when no email provider is configured, got nil")
	}
	if !strings.Contains(err.Error(), "no provider configured") {
		t.Fatalf("error should explain the misconfiguration, got %q", err)
	}
}

func TestSendNotificationEmail_MissingRecipient_ReturnsError(t *testing.T) {
	_, err := SendNotificationEmail(config.Config{}, "", "title", "body", "", "", nil, EmailOptions{})
	if err == nil {
		t.Fatal("expected an error for an empty recipient address")
	}
}

func TestSendNotificationEmail_ConfiguredButNoEmailFrom_ReturnsNamedError(t *testing.T) {
	// RESEND_API_KEY present, EMAIL_FROM empty — the exact production
	// misconfiguration that made Resend 422 every send. Must fail before the
	// provider call, with a message that names EMAIL_FROM.
	cfg := config.Config{ResendAPIKey: "re_test_key"}
	_, err := SendNotificationEmail(cfg, "customer@example.com", "Invoice INV-1 sent", "body", "", "<p>branded</p>", nil, EmailOptions{})
	if err == nil {
		t.Fatal("expected an error when a provider is configured but EMAIL_FROM is unset")
	}
	if !strings.Contains(err.Error(), "EMAIL_FROM") {
		t.Fatalf("error must name the missing variable, got %q", err)
	}
}

func TestSendViaResend_ErrorIncludesResponseBody(t *testing.T) {
	const resendBody = `{"statusCode":422,"name":"validation_error","message":"The gmail.com domain is not verified."}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(resendBody))
	}))
	defer srv.Close()

	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	_, err := sendViaResend(config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"},
		"customer@example.com", "Invoice INV-1 sent", "<p>branded</p>", "branded", nil, EmailOptions{})
	if err == nil {
		t.Fatal("expected an error on a 422 response")
	}
	if !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "domain is not verified") {
		t.Fatalf("error must carry Resend's status and response body, got %q", err)
	}
}

func TestRenderEmailHTML_IncludesLinkWhenPresent(t *testing.T) {
	html := renderEmailHTML("Invoice approved", "Invoice INV-1 was approved.", "https://app.example.com/invoices/1")
	if !strings.Contains(html, "https://app.example.com/invoices/1") {
		t.Fatalf("expected rendered HTML to include the link, got %q", html)
	}
}

func TestRenderEmailHTML_OmitsLinkWhenAbsent(t *testing.T) {
	html := renderEmailHTML("Invoice approved", "Invoice INV-1 was approved.", "")
	if strings.Contains(html, "<a href=") {
		t.Fatalf("expected no link markup when link is empty, got %q", html)
	}
}

func TestSendNotificationEmail_ProviderSetNoEmailFrom_FailsBeforeSend(t *testing.T) {
	// SMTP host set, EMAIL_FROM empty: must fail without opening an SMTP
	// connection, naming EMAIL_FROM (same guard as the Resend path).
	_, err := SendNotificationEmail(config.Config{SMTPHost: "smtp.example.com"},
		"user@example.com", "title", "body", "", "", nil, EmailOptions{})
	if err == nil || !strings.Contains(err.Error(), "EMAIL_FROM") {
		t.Fatalf("expected an EMAIL_FROM error, got %v", err)
	}
}

func TestBuildSMTPMessage_IncludesAttachment(t *testing.T) {
	msg := buildSMTPMessage("user@example.com", "from@example.com", "", "Invoice sent", "<p>See attached.</p>", "",
		[]EmailAttachment{{FileName: "INV-1.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4")}}, EmailOptions{})
	s := string(msg)
	if !strings.Contains(s, "multipart/mixed") {
		t.Fatalf("expected a multipart/mixed message when an attachment is present, got:\n%s", s)
	}
	if !strings.Contains(s, `filename="INV-1.pdf"`) {
		t.Fatalf("expected the attachment filename in the message, got:\n%s", s)
	}
	if !strings.Contains(s, "JVBERi0xLjQ") {
		t.Fatalf("expected base64-encoded attachment content in the message, got:\n%s", s)
	}
}

func TestBuildSMTPMessage_UsesGivenHTMLVerbatim(t *testing.T) {
	brandedHTML := `<html><body><p>Please find your invoice attached.</p><p>Regards,<br>Acme</p></body></html>`
	msg := buildSMTPMessage("user@example.com", "from@example.com", "", "Invoice INV-1", brandedHTML, "", nil, EmailOptions{})
	s := string(msg)
	if !strings.Contains(s, "Please find your invoice attached.") {
		t.Fatalf("expected the branded HTML body verbatim, got:\n%s", s)
	}
	if strings.Contains(s, "<h2>Invoice INV-1</h2>") {
		t.Fatalf("expected the generic <h2>title</h2> template NOT to be used when explicit HTML is given, got:\n%s", s)
	}
}

func TestBuildSMTPMessage_NoAttachment_NoText_PlainHTML(t *testing.T) {
	msg := buildSMTPMessage("user@example.com", "from@example.com", "", "Invoice sent", "<p>Hi.</p>", "", nil, EmailOptions{})
	s := string(msg)
	if strings.Contains(s, "multipart/") {
		t.Fatalf("expected a plain (non-multipart) message with no attachment and no text part, got:\n%s", s)
	}
	if !strings.Contains(s, "<p>Hi.</p>") {
		t.Fatalf("expected the HTML body present, got:\n%s", s)
	}
}

func TestBuildSMTPMessage_TextAlternativeAndReplyTo(t *testing.T) {
	msg := buildSMTPMessage("user@example.com", "from@example.com", "support@stonesuite.app",
		"You're invited", "<p>Welcome</p>", "Welcome", nil, EmailOptions{})
	s := string(msg)
	if !strings.Contains(s, "multipart/alternative") {
		t.Fatalf("expected multipart/alternative when a text part is supplied, got:\n%s", s)
	}
	if !strings.Contains(s, "text/plain; charset=UTF-8") || !strings.Contains(s, "text/html; charset=UTF-8") {
		t.Fatalf("expected both a text/plain and a text/html part, got:\n%s", s)
	}
	if !strings.Contains(s, "Reply-To: support@stonesuite.app") {
		t.Fatalf("expected a Reply-To header, got:\n%s", s)
	}
}

func TestBuildSMTPMessage_TextAlternativeInsideMixedWithAttachment(t *testing.T) {
	msg := buildSMTPMessage("user@example.com", "from@example.com", "", "Doc", "<p>See attached</p>", "See attached",
		[]EmailAttachment{{FileName: "d.pdf", ContentType: "application/pdf", Content: []byte("%PDF-1.4")}}, EmailOptions{})
	s := string(msg)
	if !strings.Contains(s, "multipart/mixed") || !strings.Contains(s, "multipart/alternative") {
		t.Fatalf("expected a multipart/alternative body nested in a multipart/mixed, got:\n%s", s)
	}
	if !strings.Contains(s, `filename="d.pdf"`) {
		t.Fatalf("expected the attachment part, got:\n%s", s)
	}
}

func TestHTMLToPlainText(t *testing.T) {
	in := `<!DOCTYPE html><html><head><style>body{color:red}</style><title>x</title></head>` +
		`<body><div style="display:none;">hidden preheader</div>` +
		`<p style="x">You're invited to join <strong>Acme</strong>.</p>` +
		`<p><a href="https://app.example/accept?token=abc" style="y">Accept invitation</a></p></body></html>`
	out := htmlToPlainText(in)

	if strings.Contains(out, "<") || strings.Contains(out, ">") {
		t.Fatalf("expected all tags stripped, got:\n%s", out)
	}
	if strings.Contains(out, "hidden preheader") {
		t.Fatalf("expected the hidden preheader dropped, got:\n%s", out)
	}
	if strings.Contains(out, "color:red") || strings.Contains(out, "<title>") {
		t.Fatalf("expected <head>/<style> dropped, got:\n%s", out)
	}
	if !strings.Contains(out, "You're invited to join Acme.") {
		t.Fatalf("expected the body text, got:\n%s", out)
	}
	if !strings.Contains(out, "Accept invitation: https://app.example/accept?token=abc") {
		t.Fatalf("expected the anchor rendered as 'label: url', got:\n%s", out)
	}
}

func TestSendViaResend_IncludesTextAndReplyTo(t *testing.T) {
	var got resendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()
	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	cfg := config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app", EmailReplyTo: "support@stonesuite.app"}
	if _, err := sendViaResend(cfg, "customer@example.com", "You're invited", "<p>Welcome</p>", "Welcome", nil, EmailOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Text != "Welcome" {
		t.Fatalf("expected the text body forwarded to Resend, got %q", got.Text)
	}
	if got.ReplyTo != "support@stonesuite.app" {
		t.Fatalf("expected reply_to forwarded to Resend, got %q", got.ReplyTo)
	}
}

func TestSendViaResend_OmitsReplyToWhenUnset(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	cfg := config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"}
	if _, err := sendViaResend(cfg, "customer@example.com", "Subject", "<p>x</p>", "x", nil, EmailOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(rawBody), "reply_to") {
		t.Fatalf("expected reply_to omitted when EmailReplyTo is empty, got:\n%s", rawBody)
	}
}

func TestSendViaResend_ReturnsProviderEmailID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`))
	}))
	defer srv.Close()

	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	id, err := sendViaResend(config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"},
		"customer@example.com", "Subject", "<p>x</p>", "x", nil, EmailOptions{})
	if err != nil {
		t.Fatalf("sendViaResend: %v", err)
	}
	if id != "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794" {
		t.Fatalf("provider id = %q, want the id from Resend's response", id)
	}
}

func TestSendViaResend_UnparseableSuccessBodyIsNotAnError(t *testing.T) {
	// The email was accepted; losing the id only means no webhook tracking.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	id, err := sendViaResend(config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"},
		"customer@example.com", "Subject", "<p>x</p>", "x", nil, EmailOptions{})
	if err != nil {
		t.Fatalf("a 2xx with a bad body must not be an error, got %v", err)
	}
	if id != "" {
		t.Fatalf("provider id = %q, want empty", id)
	}
}

func TestSendViaResend_PerMessageOptionsOverrideDefaults(t *testing.T) {
	var got resendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()
	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	cfg := config.Config{ResendAPIKey: "re_test_key", EmailFrom: "Stone Suite <orders@stonesuite.app>", EmailReplyTo: "support@stonesuite.app"}
	opts := EmailOptions{
		ReplyTo:    "so-abc123@reply.stonesuite.app",
		FromName:   "Harish (Acme Stone)",
		InReplyTo:  "<cust-1@mail.gmail.com>",
		References: "<cust-1@mail.gmail.com>",
	}
	if _, err := sendViaResend(cfg, "customer@example.com", "Re: SO-1", "<p>x</p>", "x", nil, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ReplyTo != "so-abc123@reply.stonesuite.app" {
		t.Fatalf("expected the per-message reply_to to win over EMAIL_REPLY_TO, got %q", got.ReplyTo)
	}
	if got.From != `"Harish (Acme Stone)" <orders@stonesuite.app>` {
		t.Fatalf("expected the sender name over the configured address, got %q", got.From)
	}
	if got.Headers["In-Reply-To"] != "<cust-1@mail.gmail.com>" || got.Headers["References"] != "<cust-1@mail.gmail.com>" {
		t.Fatalf("expected threading headers forwarded, got %v", got.Headers)
	}
}

func TestEmailOptions_ZeroValueKeepsDefaults(t *testing.T) {
	cfg := config.Config{EmailFrom: "no-reply@stonesuite.app", EmailReplyTo: "support@stonesuite.app"}
	var o EmailOptions
	if o.from(cfg) != "no-reply@stonesuite.app" || o.replyTo(cfg) != "support@stonesuite.app" || o.threadHeaders() != nil {
		t.Fatalf("zero options must not change the service defaults")
	}
}

func TestEmailOptions_StripsHeaderInjection(t *testing.T) {
	cfg := config.Config{EmailFrom: "no-reply@stonesuite.app"}
	o := EmailOptions{ReplyTo: "a@b.com\r\nBcc: evil@x.com", FromName: "Eve\r\nBcc: evil@x.com", InReplyTo: "<1@x>\r\nX-Evil: 1"}
	if strings.ContainsAny(o.replyTo(cfg), "\r\n") || strings.ContainsAny(o.from(cfg), "\r\n") {
		t.Fatalf("CR/LF must never survive into a header value")
	}
	for _, v := range o.threadHeaders() {
		if strings.ContainsAny(v, "\r\n") {
			t.Fatalf("CR/LF must never survive into a threading header, got %q", v)
		}
	}
}

func TestBuildSMTPMessage_WritesFromAndThreadingHeaders(t *testing.T) {
	opts := EmailOptions{FromName: "Harish", InReplyTo: "<c1@x>", References: "<c1@x>"}
	msg := string(buildSMTPMessage("u@example.com", `"Harish" <orders@stonesuite.app>`, "so-1@reply.stonesuite.app",
		"Re: SO-1", "<p>x</p>", "x", nil, opts))
	for _, want := range []string{`From: "Harish" <orders@stonesuite.app>`, "Reply-To: so-1@reply.stonesuite.app", "In-Reply-To: <c1@x>", "References: <c1@x>"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected %q in message, got:\n%s", want, msg)
		}
	}
}

func TestBuildSMTPMessage_IncludesEveryAttachmentInOrder(t *testing.T) {
	msg := string(buildSMTPMessage("user@example.com", "from@example.com", "", "Re: Your order", "<p>Hi</p>", "Hi",
		[]EmailAttachment{
			{FileName: "plan.pdf", ContentType: "application/pdf", Content: []byte("one")},
			{FileName: "kitchen.jpg", ContentType: "image/jpeg", Content: []byte("two")},
			{FileName: "notes.txt", Content: []byte("three")},
		}, EmailOptions{}))

	first := strings.Index(msg, `filename="plan.pdf"`)
	second := strings.Index(msg, `filename="kitchen.jpg"`)
	third := strings.Index(msg, `filename="notes.txt"`)
	if first < 0 || second < first || third < second {
		t.Fatalf("expected the three attachment parts in order, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Type: image/jpeg") {
		t.Errorf("expected each part to carry its own content type, got:\n%s", msg)
	}
	if !strings.Contains(msg, "Content-Type: application/octet-stream") {
		t.Errorf("expected a part with no type to default to octet-stream, got:\n%s", msg)
	}
	if got := strings.Count(msg, "Content-Disposition: attachment"); got != 3 {
		t.Errorf("attachment parts = %d, want 3", got)
	}
}

func TestBuildSMTPMessage_AFileNameCannotInjectHeaders(t *testing.T) {
	msg := string(buildSMTPMessage("user@example.com", "from@example.com", "", "Doc", "<p>x</p>", "",
		[]EmailAttachment{{
			FileName:    "a\"; x=1\r\nBcc: evil@example.com\r\n\r\nboo.pdf",
			ContentType: "application/pdf\r\nX-Evil: 1",
			Content:     []byte("x"),
		}}, EmailOptions{}))

	if strings.Contains(msg, "\r\nBcc:") || strings.Contains(msg, "\r\nX-Evil:") {
		t.Fatalf("a header was injected through a file name or content type:\n%s", msg)
	}
}

func TestSendViaResend_SendsEveryAttachmentInOrder(t *testing.T) {
	var got resendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()
	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	cfg := config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"}
	_, err := sendViaResend(cfg, "customer@example.com", "Re: Your order", "<p>x</p>", "x", []EmailAttachment{
		{FileName: "plan.pdf", ContentType: "application/pdf", Content: []byte("one")},
		{FileName: "kitchen.jpg", ContentType: "image/jpeg", Content: []byte("two")},
	}, EmailOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got.Attachments) != 2 {
		t.Fatalf("attachments sent = %d, want 2", len(got.Attachments))
	}
	if a := got.Attachments[0]; a.Filename != "plan.pdf" || a.Content != "b25l" || a.ContentType != "application/pdf" {
		t.Errorf("first attachment = %+v", a)
	}
	if a := got.Attachments[1]; a.Filename != "kitchen.jpg" || a.Content != "dHdv" || a.ContentType != "image/jpeg" {
		t.Errorf("second attachment = %+v", a)
	}
}

func TestSendViaResend_NoAttachmentsOmitsTheField(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"re_1"}`))
	}))
	defer srv.Close()
	orig := resendEndpoint
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = orig }()

	cfg := config.Config{ResendAPIKey: "re_test_key", EmailFrom: "no-reply@stonesuite.app"}
	if _, err := sendViaResend(cfg, "c@example.com", "s", "<p>x</p>", "x", nil, EmailOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(raw), "attachments") {
		t.Fatalf("a send with no files must not carry an attachments field: %s", raw)
	}
}
