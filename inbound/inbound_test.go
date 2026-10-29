package inbound

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const sampleEvent = `{
  "type": "email.received",
  "created_at": "2026-10-16T10:00:00Z",
  "data": {
    "email_id": "em_123",
    "from": "Priya Shah <priya@gmail.com>",
    "to": ["Acme Orders <r-abcdefghijklmnopqrst@reply.example.com>"],
    "cc": ["boss@example.org"],
    "bcc": ["audit@reply.example.com"],
    "message_id": "<CAF+abc@mail.gmail.com>",
    "subject": "Re: Your Sales Order SO-1",
    "attachments": [{"id": "att_1", "filename": "photo.jpg"}]
  }
}`

func TestParseEvent(t *testing.T) {
	ev, err := ParseEvent([]byte(sampleEvent))
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if ev.Type != EventEmailReceived || ev.Data.EmailID != "em_123" || len(ev.Data.Attachments) != 1 {
		t.Fatalf("unexpected parse: %+v", ev)
	}
	got := ev.Recipients()
	if len(got) != 3 || got[1] != "boss@example.org" || got[2] != "audit@reply.example.com" {
		t.Fatalf("Recipients() = %v, want to, cc, bcc in order", got)
	}
	if _, err := ParseEvent([]byte("{")); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestAddressedToDomain(t *testing.T) {
	tests := []struct {
		name  string
		addrs []string
		want  bool
	}{
		{"bare address", []string{"r-abc@reply.example.com"}, true},
		{"display name form", []string{"Acme Orders <r-abc@reply.example.com>"}, true},
		{"domain is case-insensitive", []string{"r-abc@REPLY.Example.COM"}, true},
		{"one of several", []string{"rep@acme.com", "r-abc@reply.example.com"}, true},
		{"another domain", []string{"rep@acme.com"}, false},
		{"subdomain is not the domain", []string{"r-abc@mail.reply.example.com"}, false},
		{"domain only as a suffix of the local part", []string{"reply.example.com@acme.com"}, false},
		{"unparseable entries are skipped", []string{"not an address", ""}, false},
		{"no recipients", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AddressedToDomain(tc.addrs, "reply.example.com"); got != tc.want {
				t.Fatalf("AddressedToDomain(%v) = %v, want %v", tc.addrs, got, tc.want)
			}
		})
	}
}

func TestBuildMail(t *testing.T) {
	ev, _ := ParseEvent([]byte(sampleEvent))

	t.Run("fetched copy wins and carries the threading headers", func(t *testing.T) {
		m := BuildMail(ev, Received{
			From: "Priya <priya@gmail.com>", To: []string{"r-x@reply.example.com"}, Cc: []string{"cc@x.com"},
			Subject: "Re: fetched", Text: "hello", HTML: "<p>hello</p>", MessageID: "<fetched@x>",
			Headers: map[string]any{"In-Reply-To": " <sent@resend> ", "REFERENCES": "<sent@resend>"},
		}, nil)
		if m.EmailID != "em_123" || m.From != "Priya <priya@gmail.com>" || m.Subject != "Re: fetched" || m.MessageID != "<fetched@x>" {
			t.Fatalf("fetched values not preferred: %+v", m)
		}
		if m.InReplyTo != "<sent@resend>" || m.References != "<sent@resend>" {
			t.Fatalf("threading headers = %q / %q", m.InReplyTo, m.References)
		}
		if len(m.To) != 1 || m.To[0] != "r-x@reply.example.com" || len(m.CC) != 1 || m.CC[0] != "cc@x.com" {
			t.Fatalf("recipients = %v / %v", m.To, m.CC)
		}
		if m.Text != "hello" || m.HTML != "<p>hello</p>" || m.AttachmentCount != 1 {
			t.Fatalf("body/attachments wrong: %+v", m)
		}
	})

	t.Run("webhook metadata fills what the fetched copy lacks", func(t *testing.T) {
		m := BuildMail(ev, Received{Text: "x"}, nil)
		if m.From != "Priya Shah <priya@gmail.com>" || m.Subject != "Re: Your Sales Order SO-1" || m.MessageID != "<CAF+abc@mail.gmail.com>" {
			t.Fatalf("fallbacks wrong: %+v", m)
		}
		if len(m.To) != 1 || len(m.CC) != 1 {
			t.Fatalf("recipient fallbacks wrong: %v / %v", m.To, m.CC)
		}
	})

	t.Run("bcc comes only from the webhook", func(t *testing.T) {
		m := BuildMail(ev, Received{Text: "x"}, nil)
		if len(m.BCC) != 1 || m.BCC[0] != "audit@reply.example.com" {
			t.Fatalf("BCC = %v", m.BCC)
		}
	})

	t.Run("oversized parts are cut on a character boundary", func(t *testing.T) {
		long := strings.Repeat("é", maxBodyBytes) // 2 bytes each
		m := BuildMail(ev, Received{Text: long, HTML: long}, nil)
		for name, got := range map[string]string{"text": m.Text, "html": m.HTML} {
			if len(got) > maxBodyBytes || !utf8.ValidString(got) {
				t.Errorf("%s: %d bytes, valid=%v; want <= %d and valid UTF-8", name, len(got), utf8.ValidString(got), maxBodyBytes)
			}
		}
	})
}

func TestBuildMail_ListedFilesBecomeAttachmentMetadata(t *testing.T) {
	ev, _ := ParseEvent([]byte(sampleEvent))
	files := []ReceivedAttachment{
		{ID: "att_1", Filename: "photo.jpg", Size: 4096, ContentType: "image/jpeg", ContentDisposition: "attachment", DownloadURL: "https://inbound-cdn.resend.com/x"},
		{ID: "att_2", Filename: "logo.png", Size: 300, ContentType: "image/png", ContentDisposition: "INLINE", ContentID: "img001"},
	}

	m := BuildMail(ev, Received{Text: "x"}, files)

	if len(m.Attachments) != 2 {
		t.Fatalf("attachments = %+v, want 2", m.Attachments)
	}
	if a := m.Attachments[0]; a.ID != "att_1" || a.Filename != "photo.jpg" || a.ContentType != "image/jpeg" || a.Size != 4096 || a.Inline {
		t.Errorf("first = %+v", a)
	}
	if a := m.Attachments[1]; !a.Inline {
		t.Errorf("a file with disposition INLINE must be flagged inline: %+v", a)
	}
	if m.AttachmentCount != 1 {
		t.Errorf("AttachmentCount = %d, want the webhook's 1 (it still comes from the webhook)", m.AttachmentCount)
	}

	if got := BuildMail(ev, Received{Text: "x"}, nil).Attachments; len(got) != 0 {
		t.Errorf("no listed files means no attachment metadata, got %+v", got)
	}
}

func TestProcessor_CopiesFiles(t *testing.T) {
	ev, _ := ParseEvent([]byte(sampleEvent))
	listed := []ReceivedAttachment{{ID: "att_1", Filename: "photo.jpg", Size: 10, ContentType: "image/jpeg"}}
	proc := func(f *fakeFetcher, l *fakeLister, fw *fakeForwarder, jobs *fakeJobs) Processor {
		p := Processor{Domain: "reply.example.com", Fetcher: f, Forwarder: fw}
		if l != nil {
			p.Lister = l
		}
		if jobs != nil {
			p.Jobs = jobs
		}
		return p
	}

	t.Run("lists the files, forwards their metadata and queues what the backend wants", func(t *testing.T) {
		f, l, jobs := &fakeFetcher{mail: Received{Text: "see attached"}}, &fakeLister{files: listed}, &fakeJobs{}
		fw := &fakeForwarder{result: ForwardResult{UploadRef: "r-abcdefghijklmnopqrst", Pending: []string{"att_1"}}}

		out, err := proc(f, l, fw, jobs).Process(context.Background(), ev)

		if err != nil || out != OutcomeForwarded {
			t.Fatalf("Process = %v, %v", out, err)
		}
		if l.calls != 1 || len(fw.got.Attachments) != 1 || fw.got.Attachments[0].ID != "att_1" {
			t.Fatalf("listed %d times, forwarded %+v", l.calls, fw.got.Attachments)
		}
		if len(jobs.queued) != 1 || jobs.queued[0].emailID != "em_123" || jobs.queued[0].ref != "r-abcdefghijklmnopqrst" ||
			len(jobs.queued[0].ids) != 1 || jobs.queued[0].ids[0] != "att_1" {
			t.Fatalf("queued = %+v", jobs.queued)
		}
	})

	t.Run("queues nothing when the backend wants nothing", func(t *testing.T) {
		jobs := &fakeJobs{}
		fw := &fakeForwarder{result: ForwardResult{UploadRef: "r-x", Pending: []string{}}}

		if _, err := proc(&fakeFetcher{}, &fakeLister{files: listed}, fw, jobs).Process(context.Background(), ev); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if len(jobs.queued) != 0 {
			t.Fatalf("queued %+v, want nothing", jobs.queued)
		}
	})

	t.Run("an older backend names no files, so nothing is queued", func(t *testing.T) {
		jobs := &fakeJobs{}
		if _, err := proc(&fakeFetcher{}, &fakeLister{files: listed}, &fakeForwarder{}, jobs).Process(context.Background(), ev); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if len(jobs.queued) != 0 {
			t.Fatalf("queued %+v, want nothing", jobs.queued)
		}
	})

	t.Run("without a job store the files are not listed and only the count is forwarded", func(t *testing.T) {
		l, fw := &fakeLister{files: listed}, &fakeForwarder{}
		if _, err := proc(&fakeFetcher{}, l, fw, nil).Process(context.Background(), ev); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if l.calls != 0 || len(fw.got.Attachments) != 0 || fw.got.AttachmentCount != 1 {
			t.Fatalf("listed %d times, forwarded %+v (count %d)", l.calls, fw.got.Attachments, fw.got.AttachmentCount)
		}
	})

	t.Run("a mail with no files is not listed", func(t *testing.T) {
		noFiles := ev
		noFiles.Data.Attachments = nil
		l := &fakeLister{files: listed}

		if _, err := proc(&fakeFetcher{}, l, &fakeForwarder{}, &fakeJobs{}).Process(context.Background(), noFiles); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if l.calls != 0 {
			t.Fatalf("listed %d times for a mail with no files", l.calls)
		}
	})

	t.Run("a failed listing stops before forwarding and keeps its retry mark", func(t *testing.T) {
		l, fw := &fakeLister{err: &RetryableError{Err: errors.New("resend is down")}}, &fakeForwarder{}
		_, err := proc(&fakeFetcher{}, l, fw, &fakeJobs{}).Process(context.Background(), ev)
		if err == nil || !IsRetryable(err) || fw.calls != 0 {
			t.Fatalf("err = %v retryable=%v, forwarded %d; want a retryable error and no forward", err, IsRetryable(err), fw.calls)
		}
	})

	t.Run("a queue failure after the mail was filed asks for a retry", func(t *testing.T) {
		jobs := &fakeJobs{err: errors.New("database is down")}
		fw := &fakeForwarder{result: ForwardResult{UploadRef: "r-x", Pending: []string{"att_1"}}}
		_, err := proc(&fakeFetcher{}, &fakeLister{files: listed}, fw, jobs).Process(context.Background(), ev)
		if err == nil || !IsRetryable(err) {
			t.Fatalf("err = %v retryable=%v; want a retryable error so Resend sends the webhook again", err, IsRetryable(err))
		}
		if fw.calls != 1 {
			t.Fatalf("forwarded %d times, want 1 (the mail is filed before the queue is touched)", fw.calls)
		}
	})
}

func TestTruncateBytes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"short enough", "abc", 5, "abc"},
		{"exact", "abc", 3, "abc"},
		{"ascii cut", "abcdef", 4, "abcd"},
		{"cut would split a 2-byte rune", "aé", 2, "a"},
		{"cut lands on a rune boundary", "aéb", 3, "aé"},
		{"cut would split a 3-byte rune", "a€", 3, "a"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateBytes(tc.in, tc.max); got != tc.want {
				t.Fatalf("truncateBytes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

type fakeFetcher struct {
	mail  Received
	err   error
	calls int
	gotID string
}

func (f *fakeFetcher) Fetch(_ context.Context, id string) (Received, error) {
	f.calls++
	f.gotID = id
	return f.mail, f.err
}

type fakeForwarder struct {
	err    error
	result ForwardResult
	calls  int
	got    Mail
}

func (f *fakeForwarder) Forward(_ context.Context, m Mail) (ForwardResult, error) {
	f.calls++
	f.got = m
	return f.result, f.err
}

type fakeLister struct {
	files []ReceivedAttachment
	err   error
	calls int
}

func (f *fakeLister) ListAttachments(context.Context, string) ([]ReceivedAttachment, error) {
	f.calls++
	return f.files, f.err
}

type queuedJobs struct {
	emailID, ref string
	ids          []string
}

type fakeJobs struct {
	queued []queuedJobs
	err    error
}

func (f *fakeJobs) Enqueue(_ context.Context, emailID, ref string, ids []string) error {
	if f.err != nil {
		return f.err
	}
	f.queued = append(f.queued, queuedJobs{emailID: emailID, ref: ref, ids: ids})
	return nil
}

func (f *fakeJobs) ClaimDue(context.Context, int, time.Duration) ([]Job, error) {
	return nil, nil
}

func (f *fakeJobs) Complete(context.Context, string) error {
	return nil
}

func (f *fakeJobs) Retry(context.Context, string, int, string) error {
	return nil
}

func (f *fakeJobs) Fail(context.Context, string, string) error {
	return nil
}

func TestProcessor_Process(t *testing.T) {
	ev, _ := ParseEvent([]byte(sampleEvent))
	otherDomain := strings.NewReplacer("reply.example.com", "elsewhere.org").Replace(sampleEvent)
	foreign, _ := ParseEvent([]byte(otherDomain))
	noID := ev
	noID.Data.EmailID = ""

	t.Run("forwards a reply on the reply domain", func(t *testing.T) {
		f, fw := &fakeFetcher{mail: Received{Text: "Looks good."}}, &fakeForwarder{}
		out, err := Processor{Domain: "reply.example.com", Fetcher: f, Forwarder: fw}.Process(context.Background(), ev)
		if err != nil || out != OutcomeForwarded {
			t.Fatalf("Process = %v, %v; want forwarded", out, err)
		}
		if f.gotID != "em_123" || fw.calls != 1 || fw.got.Text != "Looks good." || fw.got.EmailID != "em_123" {
			t.Fatalf("fetched %q, forwarded %d times: %+v", f.gotID, fw.calls, fw.got)
		}
	})

	for name, e := range map[string]Event{"mail for another domain": foreign, "no email id": noID} {
		t.Run("ignores "+name, func(t *testing.T) {
			f, fw := &fakeFetcher{}, &fakeForwarder{}
			out, err := Processor{Domain: "reply.example.com", Fetcher: f, Forwarder: fw}.Process(context.Background(), e)
			if err != nil || out != OutcomeIgnored {
				t.Fatalf("Process = %v, %v; want ignored", out, err)
			}
			if f.calls != 0 || fw.calls != 0 {
				t.Fatalf("fetched %d, forwarded %d; want neither", f.calls, fw.calls)
			}
		})
	}

	t.Run("a fetch failure stops before forwarding and keeps its retry mark", func(t *testing.T) {
		f, fw := &fakeFetcher{err: &RetryableError{Err: errors.New("resend is down")}}, &fakeForwarder{}
		_, err := Processor{Domain: "reply.example.com", Fetcher: f, Forwarder: fw}.Process(context.Background(), ev)
		if err == nil || !IsRetryable(err) || fw.calls != 0 {
			t.Fatalf("err = %v retryable=%v, forwarded %d; want a retryable error and no forward", err, IsRetryable(err), fw.calls)
		}
	})

	t.Run("a permanent forward failure is not retryable", func(t *testing.T) {
		f, fw := &fakeFetcher{}, &fakeForwarder{err: errors.New("bad payload")}
		_, err := Processor{Domain: "reply.example.com", Fetcher: f, Forwarder: fw}.Process(context.Background(), ev)
		if err == nil || IsRetryable(err) {
			t.Fatalf("err = %v retryable=%v; want a permanent error", err, IsRetryable(err))
		}
	})
}

func TestRetryableStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{200, false}, {400, false}, {402, false}, {413, false}, {422, false},
		{401, true}, {403, true}, {404, true}, {408, true}, {429, true},
		{500, true}, {502, true}, {503, true}, {504, true},
	}
	for _, tc := range tests {
		if got := retryableStatus(tc.status); got != tc.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}
