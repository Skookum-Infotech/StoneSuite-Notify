package channels

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"

	"stonesuite-notify/config"
)

func TestSMTPEnvelopeFrom(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"bare address is unchanged", "dev@stonesuite.test", "dev@stonesuite.test"},
		{"display name form yields the bare address", "StoneSuite <dev@stonesuite.test>", "dev@stonesuite.test"},
		{"quoted name with a comma", `"Stone, Suite" <dev@stonesuite.test>`, "dev@stonesuite.test"},
		{"angle brackets around a bare address", "<dev@stonesuite.test>", "dev@stonesuite.test"},
		{"unparseable value passes through for the server to judge", "not an address", "not an address"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := smtpEnvelopeFrom(tc.in); got != tc.want {
				t.Fatalf("smtpEnvelopeFrom(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// fakeSMTP is a one-connection SMTP server that records the MAIL FROM and
// RCPT TO commands it is given.
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	mailFrom string
	rcptTo   string
	done     chan struct{}
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{ln: ln, done: make(chan struct{})}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) serve() {
	defer close(f.done)
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	rd := bufio.NewReader(conn)
	reply := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	reply("220 fake ESMTP")
	inData := false
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				reply("250 queued")
			}
			continue
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			reply("250 fake")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.mailFrom = strings.TrimSpace(line[len("MAIL FROM:"):])
			f.mu.Unlock()
			// Like Mailpit and most servers, accept only a bare <address>.
			if strings.Contains(f.mailFrom, " ") || strings.Count(f.mailFrom, "<") > 1 {
				reply(`501 5.5.4 Syntax error in parameters or arguments (invalid FROM parameter)`)
				continue
			}
			reply("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.rcptTo = strings.TrimSpace(line[len("RCPT TO:"):])
			f.mu.Unlock()
			reply("250 ok")
		case up == "DATA":
			inData = true
			reply("354 go ahead")
		case up == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

// A sender configured as "Name <address>" — the form the README recommends and
// Resend accepts — must still be deliverable over SMTP: the MAIL FROM command
// takes the bare address only.
func TestSendViaSMTP_DisplayNameSenderUsesBareEnvelopeAddress(t *testing.T) {
	srv := newFakeSMTP(t)
	host, port, err := net.SplitHostPort(srv.ln.Addr().String())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	cfg := config.Config{
		SMTPHost:  host,
		SMTPPort:  port,
		EmailFrom: "StoneSuite <dev@stonesuite.test>",
	}

	if err := sendViaSMTP(cfg, "vendor@example.test", "subject", "<p>hi</p>", "hi", nil, EmailOptions{}); err != nil {
		t.Fatalf("sendViaSMTP with a display-name sender failed: %v", err)
	}
	<-srv.done

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.mailFrom != "<dev@stonesuite.test>" {
		t.Fatalf("MAIL FROM = %q, want the bare <dev@stonesuite.test>", srv.mailFrom)
	}
	if srv.rcptTo != "<vendor@example.test>" {
		t.Fatalf("RCPT TO = %q, want <vendor@example.test>", srv.rcptTo)
	}
}
