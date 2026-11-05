# Transcript Dock for Go

A Go client for the [Transcript Dock API](https://www.transcriptdock.com/docs).
Uses the standard library with no external dependencies. Go 1.22 or newer.

## Install

```sh
go get github.com/TranscriptDock/transcriptdock-go@v0.1.0
```

## Submit and read a transcript

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "time"

    transcriptdock "github.com/TranscriptDock/transcriptdock-go"
)

func main() {
    client, err := transcriptdock.New(os.Getenv("TRANSCRIPTDOCK_API_KEY"))
    if err != nil { log.Fatal(err) }
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
    defer cancel()

    // Generate once per intended operation and save the key and returned ID.
    // Reuse the exact body and key to recover a lost submission response.
    job, err := client.CreateJob(ctx, transcriptdock.Object{
        "source": transcriptdock.Object{"url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ"},
        "mode": "captions_only", "max_credits": 1,
    }, "replace-with-your-unique-operation-key")
    if err != nil { log.Fatal(err) }
    job, err = client.WaitForJob(ctx, job["id"].(string), 3*time.Second)
    if err != nil { log.Printf("Last job: %v; %v", job, err); return }
    if job["status"] != "succeeded" { log.Printf("Job ended: %v", job); return }
    text, err := client.ExportTranscript(ctx, job["result_id"].(string), "txt")
    if err != nil { log.Fatal(err) }
    fmt.Println(text)
}
```

Creating a job can spend credits. The example requests captions only with a
one credit ceiling. It does not enable AI fallback. Check account capabilities
before using other modes. Never blindly resubmit after a network error.

## API coverage

Convenience methods: `Capabilities`, `CreateJob`, `GetJob`, `GetTranscript`,
`ExportTranscript`, `WaitForJob`. Every method takes a context. Polling requires
a context deadline and returns the last job on timeout. Failed and cancelled
jobs are returned normally; inspect status before reading `result_id`.

`Do(ctx, method, path, query, body, idempotencyKey)` covers the complete public
API, including batches, video/language discovery, upload slots, usage and
webhook endpoints. Pass `nil` for absent query/body values and an empty string
for requests without an idempotency key. For example:

```go
jobs, err := client.Do(ctx, "GET", "/v1/jobs", url.Values{"limit": {"10"}}, nil, "")
// Import net/url for url.Values.
```

Bodies use the exact fields in the
[OpenAPI contract](https://www.transcriptdock.com/docs/openapi.yaml).
Responses are `Object` maps preserving unknown fields and raw timing units.
Export returns the original string for JSON, TXT, SRT or VTT.

`Do` rejects absolute URLs and traversal segments. Redirects are blocked to
protect bearer credentials. The client timeout is 30 seconds per request;
context deadlines can shorten it. No application retry loop is used. Submission
request bodies cannot be replayed automatically by the HTTP transport. For
error handling, use `errors.As` with `*transcriptdock.APIError` to inspect status,
code, retryability, retry delay and request ID. API messages redact bearer tokens.

Use `NewWithBaseURL` for another HTTPS origin or loopback HTTP development.
Signed storage upload URLs must be used separately without the API key.

## Development

```sh
go test ./...
go vet ./...
```

Tests use a local fixture server and spend no credits. There is no `go.sum`
because there are no external dependencies. A release is an immutable version
tag in this branded repository, followed by a Go proxy indexing request.
