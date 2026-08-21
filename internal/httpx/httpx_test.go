package httpx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetRetriesRetryableStatus(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) < 3 {
			writer.Header().Set("Retry-After", "0")
			http.Error(writer, "temporary", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var sleeps atomic.Int32
	client := New(Options{
		MetadataDoer: server.Client(),
		Sleeper: SleepFunc(func(context.Context, time.Duration) error {
			sleeps.Add(1)
			return nil
		}),
		Retry: RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(context.Background(), request, RequestOptions{Kind: Metadata, Retry: true, Operation: "list"})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || calls.Load() != 3 || sleeps.Load() != 2 {
		t.Fatalf("status=%d calls=%d sleeps=%d", response.StatusCode, calls.Load(), sleeps.Load())
	}
}

func TestWriteDoesNotRetryAndMarksUnknown(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	doer := doerFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("request to https://x/?token=secret failed")
	})
	client := New(Options{MetadataDoer: doer})
	request, err := http.NewRequest(http.MethodPost, "https://example.test/?token=secret", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(context.Background(), request, RequestOptions{Kind: Metadata, Retry: false, Operation: "create"})
	if err == nil || !IsUnknown(err) {
		t.Fatalf("error = %v, want unknown write error", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("error leaked token: %v", err)
	}
}

func TestCheckResponseBoundsErrorBody(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodGet, "https://example.test/?token=secret", nil)
	response := &http.Response{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       io.NopCloser(strings.NewReader("Authorization: token secret\n" + strings.Repeat("x", 100))),
		Request:    request,
		Header:     make(http.Header),
	}
	client := New(Options{MaxErrorBody: 10})
	err := client.CheckResponse(response, "list")
	if err == nil {
		t.Fatal("CheckResponse() error = nil")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "Authorization") || len(err.Error()) > 200 {
		t.Fatalf("unsafe/unbounded error: %q", err)
	}
}

func TestReadRetriesInterruptedResponseBody(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	client := New(Options{
		MetadataDoer: doerFunc(func(request *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     make(http.Header),
					Body:       failingReadCloser{},
					Request:    request,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("complete")),
				Request:    request,
			}, nil
		}),
		Sleeper: SleepFunc(func(context.Context, time.Duration) error { return nil }),
		Retry:   RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})
	var result bytes.Buffer
	err := client.Read(
		context.Background(),
		ReadOptions{
			RequestOptions: RequestOptions{Kind: Metadata, Retry: true, Operation: "read"},
			BeforeAttempt: func(attempt int) error {
				if attempt > 1 {
					result.Reset()
				}
				return nil
			},
		},
		func(ctx context.Context) (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/download", nil)
		},
		func(response *http.Response) error {
			_, err := io.Copy(&result, response.Body)
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || result.String() != "complete" {
		t.Fatalf("calls=%d result=%q", calls.Load(), result.String())
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

type failingReadCloser struct{}

func (failingReadCloser) Read(buffer []byte) (int, error) {
	copy(buffer, "partial")
	return len("partial"), io.ErrUnexpectedEOF
}

func (failingReadCloser) Close() error { return nil }
