package inbound

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
