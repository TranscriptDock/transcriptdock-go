package transcriptdock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubmissionAndRedirectProtection(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer td_live_fixture_only" {
			t.Error("missing authorization")
		}
		if r.URL.Path == "/v1/redirect" {
			http.Redirect(w, r, "/v1/leak", http.StatusFound)
			return
		}
		if r.Header.Get("Idempotency-Key") != "fixture-replay-01" {
			t.Error("missing replay key")
		}
		var body Object
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["mode"] != "captions_only" || body["max_credits"] != float64(1) {
			t.Error("submission body changed")
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"id":"fixture","status":"queued","future_field":42}`))
	}))
	defer srv.Close()
	c, err := NewWithBaseURL("td_live_fixture_only", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, err := c.CreateJob(context.Background(), Object{"source": Object{"url": "https://example.com/video"}, "mode": "captions_only", "max_credits": 1}, "fixture-replay-01")
		if err != nil || out["future_field"] != float64(42) {
			t.Fatalf("response %v %v", out, err)
		}
	}
	_, err = c.Do(context.Background(), http.MethodGet, "/v1/redirect", nil, nil, "")
	var api *APIError
	if !errors.As(err, &api) || api.Status != 302 || calls != 3 {
		t.Fatalf("redirect followed or error lost: %d %v", calls, err)
	}
}

func TestErrorRedactionAndNoApplicationRetry(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"td_live_fixture_only","retryable":true,"retry_after_seconds":4},"request_id":"fixture-request"}`))
	}))
	defer srv.Close()
	c, _ := NewWithBaseURL("td_live_fixture_only", srv.URL)
	_, err := c.Capabilities(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Status != 429 || !api.Retryable || api.RetryAfterSeconds != 4 || strings.Contains(err.Error(), "td_live_fixture_only") || calls != 1 {
		t.Fatalf("unsafe error/retry: %v %d", err, calls)
	}
}

func TestValidationAndBoundedPolling(t *testing.T) {
	for _, base := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/path", "https://example.com?q=x"} {
		if _, err := NewWithBaseURL("fixture-key", base); err == nil {
			t.Fatal("accepted unsafe origin", base)
		}
	}
	if _, err := New("bad\nkey"); err == nil {
		t.Fatal("accepted header injection")
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"id":"fixture","status":"processing"}`))
	}))
	defer srv.Close()
	c, _ := NewWithBaseURL("fixture-key", srv.URL)
	for _, key := range []string{"", "short", "bad\nreplay"} {
		if _, err := c.CreateJob(context.Background(), Object{}, key); err == nil {
			t.Fatal("accepted unsafe replay key")
		}
	}
	if _, err := c.Do(context.Background(), "GET", "/v1/../secret", nil, nil, ""); err == nil {
		t.Fatal("accepted traversal")
	}
	if calls != 0 {
		t.Fatal("validation made a request")
	}
	if _, err := c.WaitForJob(context.Background(), "fixture", time.Second); err == nil {
		t.Fatal("unbounded polling accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	last, err := c.WaitForJob(ctx, "fixture", time.Second)
	if !errors.Is(err, context.DeadlineExceeded) || last["id"] != "fixture" {
		t.Fatalf("lost recovery state: %v %v", last, err)
	}
}
