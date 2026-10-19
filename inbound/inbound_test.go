package inbound

import (
	"context"
	"errors"
	"strings"
	"testing"
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
		})
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
		m := BuildMail(ev, Received{Text: "x"})
		if m.From != "Priya Shah <priya@gmail.com>" || m.Subject != "Re: Your Sales Order SO-1" || m.MessageID != "<CAF+abc@mail.gmail.com>" {
			t.Fatalf("fallbacks wrong: %+v", m)
		}
		if len(m.To) != 1 || len(m.CC) != 1 {
			t.Fatalf("recipient fallbacks wrong: %v / %v", m.To, m.CC)
		}
	})

	t.Run("bcc comes only from the webhook", func(t *testing.T) {
		m := BuildMail(ev, Received{Text: "x"})
		if len(m.BCC) != 1 || m.BCC[0] != "audit@reply.example.com" {
			t.Fatalf("BCC = %v", m.BCC)
		}
	})

	t.Run("oversized parts are cut on a character boundary", func(t *testing.T) {
		long := strings.Repeat("é", maxBodyBytes) // 2 bytes each
		m := BuildMail(ev, Received{Text: long, HTML: long})
		for name, got := range map[string]string{"text": m.Text, "html": m.HTML} {
			if len(got) > maxBodyBytes || !utf8.ValidString(got) {
				t.Errorf("%s: %d bytes, valid=%v; want <= %d and valid UTF-8", name, len(got), utf8.ValidString(got), maxBodyBytes)
			}
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
	err   error
	calls int
	got   Mail
}

func (f *fakeForwarder) Forward(_ context.Context, m Mail) error {
	f.calls++
	f.got = m
	return f.err
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
