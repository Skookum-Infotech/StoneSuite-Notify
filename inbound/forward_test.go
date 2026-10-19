package inbound

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

var forwardNow = time.Unix(1_790_000_000, 0)

// expectedSignature recomputes the backend's documented signature formula
// independently of sign(), so the two cannot drift together unnoticed:
// hex(HMAC-SHA256(secret, method + " " + path + "\n" + ts + "\n" + body)).
func expectedSignature(secret, method, path, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(method + " " + path + "\n" + ts + "\n"))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestHTTPForwarder_SignsAndPostsTheMail(t *testing.T) {
	var gotMethod, gotPath, gotTS, gotSig, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		gotTS, gotSig = r.Header.Get("X-Automation-Timestamp"), r.Header.Get("X-Automation-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	f := HTTPForwarder{BackendURL: srv.URL, Secret: "shared-secret", Now: func() time.Time { return forwardNow }}
	mail := Mail{EmailID: "em_1", From: "Priya <p@x.com>", To: []string{"r-a@reply.example.com"}, Text: "hi", AttachmentCount: 2}

	if err := f.Forward(context.Background(), mail); err != nil {
		t.Fatalf("Forward: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/api/internal/email/inbound" || gotType != "application/json" {
		t.Fatalf("request = %s %s (%s)", gotMethod, gotPath, gotType)
	}
	wantTS := strconv.FormatInt(forwardNow.Unix(), 10)
	if gotTS != wantTS {
		t.Fatalf("timestamp = %q, want %q", gotTS, wantTS)
	}
	if want := expectedSignature("shared-secret", http.MethodPost, gotPath, gotTS, gotBody); gotSig != want {
		t.Fatalf("signature = %q, want %q", gotSig, want)
	}
	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if decoded["emailId"] != "em_1" || decoded["text"] != "hi" || decoded["attachmentCount"] != float64(2) {
		t.Fatalf("body = %s", gotBody)
	}
}

func TestHTTPForwarder_SignsThePathItActuallyCalls(t *testing.T) {
	var gotPath, gotTS, gotSig string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTS, gotSig = r.URL.Path, r.Header.Get("X-Automation-Timestamp"), r.Header.Get("X-Automation-Signature")
		gotBody, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	// A backend served under a path prefix: the signed path must include it,
	// because the backend verifies r.URL.Path.
	f := HTTPForwarder{BackendURL: srv.URL + "/stonesuite/", Secret: "s", Now: func() time.Time { return forwardNow }}
	if err := f.Forward(context.Background(), Mail{EmailID: "em_1"}); err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if gotPath != "/stonesuite/api/internal/email/inbound" {
		t.Fatalf("path = %q", gotPath)
	}
	if want := expectedSignature("s", http.MethodPost, gotPath, gotTS, gotBody); gotSig != want {
		t.Fatalf("signature does not cover the called path: got %q, want %q", gotSig, want)
	}
}

func TestHTTPForwarder_Failures(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		retryable bool
	}{
		{"backend error", http.StatusInternalServerError, true},
		{"backend still starting", http.StatusBadGateway, true},
		{"not configured yet", http.StatusServiceUnavailable, true},
		{"wrong secret", http.StatusUnauthorized, true},
		{"route not live yet", http.StatusNotFound, true},
		{"rejected payload", http.StatusBadRequest, false},
		{"payload too large", http.StatusRequestEntityTooLarge, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"success":false,"message":"nope"}`, tc.status)
			}))
			defer srv.Close()

			err := HTTPForwarder{BackendURL: srv.URL, Secret: "s"}.Forward(context.Background(), Mail{EmailID: "em_1"})
			if err == nil {
				t.Fatal("expected an error for a non-2xx answer")
			}
			if IsRetryable(err) != tc.retryable {
				t.Fatalf("IsRetryable = %v, want %v (%v)", IsRetryable(err), tc.retryable, err)
			}
		})
	}

	t.Run("an unreachable backend can be retried", func(t *testing.T) {
		err := HTTPForwarder{BackendURL: "http://127.0.0.1:1", Secret: "s"}.Forward(context.Background(), Mail{EmailID: "em_1"})
		if err == nil || !IsRetryable(err) {
			t.Fatalf("err = %v retryable=%v; want a retryable network error", err, IsRetryable(err))
		}
	})
}
