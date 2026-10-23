package inbound

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResendFetcher_Fetch(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/receiving/missing":
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case "/receiving/denied":
			http.Error(w, `{"message":"restricted api key"}`, http.StatusForbidden)
		case "/receiving/gone":
			http.Error(w, `{"message":"unprocessable"}`, http.StatusUnprocessableEntity)
		case "/receiving/garbage":
			_, _ = w.Write([]byte(`not json`))
		default:
			_, _ = w.Write([]byte(`{"id":"em_123","from":"a@b.com","text":"hello","message_id":"<m@x>","headers":{"In-Reply-To":"<p@x>"}}`))
		}
	}))
	defer srv.Close()

	c := ResendFetcher{APIKey: "re_key", BaseURL: srv.URL + "/receiving/"}

	t.Run("loads the mail with the bearer key", func(t *testing.T) {
		got, err := c.Fetch(context.Background(), "em_123")
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if gotPath != "/receiving/em_123" || gotAuth != "Bearer re_key" {
			t.Fatalf("request path/auth = %q / %q", gotPath, gotAuth)
		}
		if got.Text != "hello" || got.Header("in-reply-to") != "<p@x>" {
			t.Fatalf("decoded wrong: %+v", got)
		}
	})

	t.Run("escapes the id in the path", func(t *testing.T) {
		if _, err := c.Fetch(context.Background(), "a/b c"); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if gotPath != "/receiving/a%2Fb%20c" {
			t.Fatalf("path = %q, want the id escaped", gotPath)
		}
	})

	failures := []struct {
		name      string
		id        string
		retryable bool
		contains  string
	}{
		{"not found can be retried", "missing", true, "404"},
		{"a restricted key can be fixed while Resend retries", "denied", true, "403"},
		{"another client error is permanent", "gone", false, "422"},
		{"a body that is not JSON is permanent", "garbage", false, "decode"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Fetch(context.Background(), tc.id)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err = %v, want one containing %q", err, tc.contains)
			}
			if IsRetryable(err) != tc.retryable {
				t.Fatalf("IsRetryable = %v, want %v", IsRetryable(err), tc.retryable)
			}
		})
	}

	t.Run("an unreachable Resend can be retried", func(t *testing.T) {
		down := ResendFetcher{APIKey: "k", BaseURL: "http://127.0.0.1:1/receiving/"}
		_, err := down.Fetch(context.Background(), "em_1")
		if err == nil || !IsRetryable(err) {
			t.Fatalf("err = %v retryable=%v; want a retryable network error", err, IsRetryable(err))
		}
	})
}

func TestResendFetcher_ListAttachments(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/receiving/em_down/attachments":
			http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
		case "/receiving/em_bad/attachments":
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		case "/receiving/em_garbage/attachments":
			_, _ = w.Write([]byte(`not json`))
		default:
			_, _ = w.Write([]byte(`{"object":"list","has_more":false,"data":[
				{"id":"att_1","filename":"plan.pdf","size":2048,"content_type":"application/pdf",
				 "content_disposition":"attachment","content_id":"","download_url":"https://inbound-cdn.resend.com/a?sig=1","expires_at":"2026-10-17T14:29:41.521Z"},
				{"id":"att_2","filename":"logo.png","size":300,"content_type":"image/png",
				 "content_disposition":"inline","content_id":"img001","download_url":"https://inbound-cdn.resend.com/b?sig=2","expires_at":"2026-10-17T14:29:41.521Z"}]}`))
		}
	}))
	defer srv.Close()
	c := ResendFetcher{APIKey: "re_key", BaseURL: srv.URL + "/receiving/"}

	t.Run("lists every file with the bearer key", func(t *testing.T) {
		got, err := c.ListAttachments(context.Background(), "em_123")
		if err != nil {
			t.Fatalf("ListAttachments: %v", err)
		}
		if gotPath != "/receiving/em_123/attachments" || gotAuth != "Bearer re_key" {
			t.Fatalf("request path/auth = %q / %q", gotPath, gotAuth)
		}
		if len(got) != 2 {
			t.Fatalf("got %d files, want 2", len(got))
		}
		if a := got[0]; a.ID != "att_1" || a.Filename != "plan.pdf" || a.Size != 2048 || a.ContentType != "application/pdf" || a.IsInline() ||
			a.DownloadURL != "https://inbound-cdn.resend.com/a?sig=1" {
			t.Errorf("first file decoded wrong: %+v", a)
		}
		if a := got[1]; !a.IsInline() || a.ContentID != "img001" {
			t.Errorf("second file should be inline: %+v", a)
		}
	})

	t.Run("escapes the mail id", func(t *testing.T) {
		if _, err := c.ListAttachments(context.Background(), "a/b c"); err != nil {
			t.Fatalf("ListAttachments: %v", err)
		}
		if gotPath != "/receiving/a%2Fb%20c/attachments" {
			t.Fatalf("path = %q", gotPath)
		}
	})

	for _, tc := range []struct {
		name      string
		id        string
		retryable bool
		contains  string
	}{
		{"an unavailable API can be retried", "em_down", true, "503"},
		{"a rejected request is permanent", "em_bad", false, "400"},
		{"a body that is not JSON is permanent", "em_garbage", false, "decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ListAttachments(context.Background(), tc.id)
			if err == nil || !strings.Contains(err.Error(), tc.contains) || IsRetryable(err) != tc.retryable {
				t.Fatalf("err = %v retryable=%v, want one containing %q with retryable=%v", err, IsRetryable(err), tc.contains, tc.retryable)
			}
		})
	}
}

func TestDefaultDownloadURLCheck(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://inbound-cdn.resend.com/file?sig=1", true},
		{"https://INBOUND-CDN.RESEND.COM/file", true},
		{"http://inbound-cdn.resend.com/file", false},
		{"https://resend.com.evil.example/file", false},
		{"https://evil.example/resend.com", false},
		{"https://notresend.com/file", false},
		{"https://169.254.169.254/latest/meta-data", false},
		{"https://localhost/file", false},
		{"ftp://inbound-cdn.resend.com/file", false},
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if got := defaultDownloadURLCheck(u) == nil; got != tc.want {
			t.Errorf("defaultDownloadURLCheck(%q) allowed = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestResendFetcher_Download(t *testing.T) {
	file := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	var gotAuth string
	var chunked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/expired":
			http.Error(w, "expired", http.StatusForbidden)
		case "/missing":
			http.Error(w, "gone", http.StatusNotFound)
		case "/weird":
			http.Error(w, "weird", http.StatusUnprocessableEntity)
		case "/cut":
			// Promises more than it sends, then hangs up.
			w.Header().Set("Content-Length", strconv.Itoa(len(file)*2))
			_, _ = w.Write(file[:10])
			if h, ok := w.(http.Hijacker); ok {
				conn, _, _ := h.Hijack()
				_ = conn.Close()
			}
		default:
			if chunked.Load() {
				if f, ok := w.(http.Flusher); ok {
					_, _ = w.Write(file[:500])
					f.Flush()
					_, _ = w.Write(file[500:])
					return
				}
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(file)))
			_, _ = w.Write(file)
		}
	}))
	defer srv.Close()
	c := ResendFetcher{APIKey: "re_key", CheckDownloadURL: func(*url.URL) error { return nil }}

	t.Run("downloads the bytes without sending the API key", func(t *testing.T) {
		got, err := c.Download(context.Background(), srv.URL+"/file", 4096)
		if err != nil || !bytes.Equal(got, file) {
			t.Fatalf("Download = %d bytes, %v; want the file", len(got), err)
		}
		if gotAuth != "" {
			t.Fatalf("the download link needs no credentials, but sent %q", gotAuth)
		}
		if cap(got) != len(file) {
			t.Errorf("cap = %d, want %d: a declared length must size the buffer once", cap(got), len(file))
		}
	})

	t.Run("reads a body of undeclared length", func(t *testing.T) {
		chunked.Store(true)
		defer chunked.Store(false)
		got, err := c.Download(context.Background(), srv.URL+"/file", 4096)
		if err != nil || !bytes.Equal(got, file) {
			t.Fatalf("Download = %d bytes, %v; want the file", len(got), err)
		}
	})

	t.Run("a file over the limit is refused", func(t *testing.T) {
		if _, err := c.Download(context.Background(), srv.URL+"/file", 999); !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("err = %v, want ErrFileTooLarge (declared length)", err)
		}
		chunked.Store(true)
		defer chunked.Store(false)
		if _, err := c.Download(context.Background(), srv.URL+"/file", 999); !errors.Is(err, ErrFileTooLarge) {
			t.Fatalf("err = %v, want ErrFileTooLarge (undeclared length)", err)
		}
		if IsRetryable(ErrFileTooLarge) {
			t.Fatal("a file that is too large must not be retried")
		}
	})

	for _, tc := range []struct {
		name      string
		path      string
		retryable bool
	}{
		{"an expired link can be retried with a fresh one", "/expired", true},
		{"a missing file can be retried", "/missing", true},
		{"another client error is permanent", "/weird", false},
		{"a body cut short can be retried", "/cut", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Download(context.Background(), srv.URL+tc.path, 4096)
			if err == nil || IsRetryable(err) != tc.retryable {
				t.Fatalf("err = %v retryable=%v, want retryable=%v", err, IsRetryable(err), tc.retryable)
			}
		})
	}

	t.Run("a link on another host is refused before any request", func(t *testing.T) {
		strict := ResendFetcher{APIKey: "k"} // the default check
		_, err := strict.Download(context.Background(), srv.URL+"/file", 4096)
		if err == nil || IsRetryable(err) {
			t.Fatalf("err = %v retryable=%v; want a permanent refusal", err, IsRetryable(err))
		}
	})

	t.Run("an unreachable host can be retried", func(t *testing.T) {
		_, err := c.Download(context.Background(), "http://127.0.0.1:1/file", 4096)
		if err == nil || !IsRetryable(err) {
			t.Fatalf("err = %v retryable=%v; want a retryable network error", err, IsRetryable(err))
		}
	})
}
