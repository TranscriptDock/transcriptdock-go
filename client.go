// Package transcriptdock calls the Transcript Dock video transcription API.
// See https://www.transcriptdock.com/docs for request fields and credit costs.
package transcriptdock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Object preserves all fields returned by the API, including future additions.
type Object map[string]any

// APIError describes a rejected API request. No application retries are performed.
type APIError struct {
	Code              string
	Message           string
	Status            int
	Retryable         bool
	RequestID         string
	RetryAfterSeconds int
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Client holds an API key and a redirect-blocking HTTP client.
type Client struct {
	key, base string
	http      *http.Client
}

// New creates a client for the canonical production origin.
func New(apiKey string) (*Client, error) {
	return NewWithBaseURL(apiKey, "https://www.transcriptdock.com")
}

// NewWithBaseURL allows HTTPS origins or loopback HTTP for local development.
func NewWithBaseURL(apiKey, baseURL string) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("provide an API key")
	}
	for _, c := range apiKey {
		if c < 33 || c > 126 {
			return nil, errors.New("API keys cannot contain whitespace or control characters")
		}
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.New("provide a valid API origin")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Hostname() == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopback)) || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("use an HTTPS origin without credentials, path, query or fragment; loopback HTTP is allowed")
	}
	return &Client{key: apiKey, base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

var tokenPattern = regexp.MustCompile(`(?i)td_live_[A-Za-z0-9_-]+|Bearer\s+\S+`)

func (c *Client) redact(s string) string {
	return tokenPattern.ReplaceAllString(strings.ReplaceAll(s, c.key, "[REDACTED]"), "[REDACTED]")
}
func validKey(s string) bool {
	if len(s) < 8 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}
func idPath(id string) (string, error) {
	if id == "" || id == "." || id == ".." {
		return "", errors.New("provide a nonempty ID")
	}
	return url.PathEscape(id), nil
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any, key string) ([]byte, error) {
	if !strings.HasPrefix(path, "/v1/") || strings.ContainsAny(path, "?#\\") {
		return nil, errors.New("use a /v1/ path and pass query parameters separately")
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("API paths cannot contain traversal segments")
		}
	}
	needsKey := method == http.MethodPost && (path == "/v1/jobs" || path == "/v1/batches" || path == "/v1/video-discoveries" || path == "/v1/language-discoveries" || strings.HasSuffix(path, "/retry"))
	if (needsKey || key != "") && !validKey(key) {
		return nil, errors.New("provide an idempotency key with 8 to 128 printable ASCII characters")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("request body must be JSON serializable")
		}
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("could not construct the API request")
	}
	// Disable replay of submission bodies by the HTTP transport.
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "transcriptdock-go/0.1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &APIError{Code: "NETWORK_ERROR", Message: "Could not reach the API. Check the job before retrying a submission.", Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &APIError{Code: "NETWORK_ERROR", Message: "Could not read the API response.", Retryable: true}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code              string `json:"code"`
				Message           string `json:"message"`
				Retryable         *bool  `json:"retryable"`
				RetryAfterSeconds int    `json:"retry_after_seconds"`
			} `json:"error"`
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(raw, &envelope)
		e := &APIError{Code: envelope.Error.Code, Message: envelope.Error.Message, Status: resp.StatusCode, RequestID: c.redact(envelope.RequestID), RetryAfterSeconds: envelope.Error.RetryAfterSeconds, Retryable: resp.StatusCode == 429 || resp.StatusCode >= 500}
		if e.Code == "" {
			e.Code = "HTTP_ERROR"
		}
		if e.Message == "" {
			e.Message = fmt.Sprintf("Request failed with status %d.", resp.StatusCode)
		}
		if envelope.Error.Retryable != nil {
			e.Retryable = *envelope.Error.Retryable
		}
		e.Code = c.redact(e.Code)
		e.Message = c.redact(e.Message)
		return nil, e
	}
	return raw, nil
}

// Do calls a public JSON endpoint with exact API field names. It preserves unknown response fields.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, idempotencyKey string) (Object, error) {
	raw, err := c.request(ctx, method, path, query, body, idempotencyKey)
	if err != nil {
		return nil, err
	}
	var out Object
	if json.Unmarshal(raw, &out) != nil || out == nil {
		return nil, &APIError{Code: "INVALID_RESPONSE", Message: "The API did not return a JSON object."}
	}
	return out, nil
}

// Capabilities returns the features currently available to the account.
func (c *Client) Capabilities(ctx context.Context) (Object, error) {
	return c.Do(ctx, http.MethodGet, "/v1/capabilities", nil, nil, "")
}

// CreateJob submits an exact API body with an explicit replay key.
func (c *Client) CreateJob(ctx context.Context, body Object, key string) (Object, error) {
	return c.Do(ctx, http.MethodPost, "/v1/jobs", nil, body, key)
}

// GetJob reads a job in the authenticated workspace.
func (c *Client) GetJob(ctx context.Context, id string) (Object, error) {
	v, err := idPath(id)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, http.MethodGet, "/v1/jobs/"+v, nil, nil, "")
}

// GetTranscript reads a transcript, retaining original timing units.
func (c *Client) GetTranscript(ctx context.Context, id string) (Object, error) {
	v, err := idPath(id)
	if err != nil {
		return nil, err
	}
	return c.Do(ctx, http.MethodGet, "/v1/transcripts/"+v, nil, nil, "")
}

// ExportTranscript returns the raw JSON, TXT, SRT or VTT export body.
func (c *Client) ExportTranscript(ctx context.Context, id, format string) (string, error) {
	if format != "json" && format != "txt" && format != "srt" && format != "vtt" {
		return "", errors.New("choose json, txt, srt or vtt")
	}
	v, err := idPath(id)
	if err != nil {
		return "", err
	}
	raw, err := c.request(ctx, http.MethodGet, "/v1/transcripts/"+v+"/export", url.Values{"format": {format}}, nil, "")
	return string(raw), err
}

// WaitForJob requires a context deadline. It returns the last job on cancellation or timeout.
// Failed and cancelled terminal jobs are returned normally; callers must inspect status.
func (c *Client) WaitForJob(ctx context.Context, id string, interval time.Duration) (Object, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("set a context deadline before polling")
	}
	if interval <= 0 {
		return nil, errors.New("poll interval must be positive")
	}
	var last Object
	for {
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
		job, err := c.GetJob(ctx, id)
		if err != nil {
			return last, err
		}
		last = job
		if job["status"] == "succeeded" || job["status"] == "failed" || job["status"] == "cancelled" {
			return job, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, ctx.Err()
		case <-timer.C:
		}
	}
}
