// Package httpx provides safe, testable HTTP execution and retry behavior.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const defaultMaxErrorBody = 64 << 10

// Kind selects the timeout profile to use.
type Kind int

const (
	Metadata Kind = iota
	Download
)

// Doer is satisfied by http.Client and makes HTTP behavior injectable in tests.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Sleeper makes retry and Gitee consistency waits testable.
type Sleeper interface {
	Sleep(context.Context, time.Duration) error
}

// SleepFunc adapts a function to Sleeper.
type SleepFunc func(context.Context, time.Duration) error

func (f SleepFunc) Sleep(ctx context.Context, delay time.Duration) error { return f(ctx, delay) }

// RetryPolicy bounds retries for idempotent read requests.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Jitter      func(time.Duration) time.Duration
}

// RequestOptions describes one outgoing request.
type RequestOptions struct {
	Kind      Kind
	Retry     bool
	Operation string
}

// ReadOptions configures a complete safe read operation. BeforeAttempt is used
// by streaming callers to reset any partial destination before a retry.
type ReadOptions struct {
	RequestOptions
	BeforeAttempt func(int) error
}

// RequestFactory creates a fresh request for every read attempt.
type RequestFactory func(context.Context) (*http.Request, error)

// ResponseConsumer consumes a successful response body.
type ResponseConsumer func(*http.Response) error

// Options configures a Client.
type Options struct {
	MetadataDoer Doer
	DownloadDoer Doer
	UserAgent    string
	Retry        RetryPolicy
	Sleeper      Sleeper
	MaxErrorBody int64
}

// Client wraps two HTTP timeout profiles and safe response handling.
type Client struct {
	metadata     Doer
	download     Doer
	userAgent    string
	retry        RetryPolicy
	sleeper      Sleeper
	maxErrorBody int64
}

// OpError is safe to log: it never embeds authorization headers or raw
// transport error text that may contain sensitive URLs.
type OpError struct {
	Operation  string
	Method     string
	URL        string
	Status     int
	Retryable  bool
	Unknown    bool
	RetryAfter time.Duration
	Summary    string
	Err        error
}

func (e *OpError) Error() string {
	operation := e.Operation
	if operation == "" {
		operation = "HTTP request"
	}
	details := e.Summary
	if details == "" {
		details = "request failed"
	}
	if e.Status != 0 {
		return fmt.Sprintf("%s failed (%s %s, status=%d): %s", operation, e.Method, e.URL, e.Status, details)
	}
	return fmt.Sprintf("%s failed (%s %s): %s", operation, e.Method, e.URL, details)
}

func (e *OpError) Unwrap() error { return e.Err }

// IsUnknown reports whether a write request might have been accepted remotely
// even though the caller did not receive a response.
func IsUnknown(err error) bool {
	var opErr *OpError
	return errors.As(err, &opErr) && opErr.Unknown
}

// New creates an HTTP client with separate metadata and download timeouts.
func New(options Options) *Client {
	if options.MetadataDoer == nil {
		options.MetadataDoer = &http.Client{Timeout: 45 * time.Second}
	}
	if options.DownloadDoer == nil {
		options.DownloadDoer = &http.Client{Timeout: 15 * time.Minute}
	}
	if options.UserAgent == "" {
		options.UserAgent = "release2gitee/dev"
	}
	if options.Retry.MaxAttempts <= 0 {
		options.Retry.MaxAttempts = 3
	}
	if options.Retry.BaseDelay <= 0 {
		options.Retry.BaseDelay = 250 * time.Millisecond
	}
	if options.Retry.MaxDelay <= 0 {
		options.Retry.MaxDelay = 3 * time.Second
	}
	if options.Retry.Jitter == nil {
		options.Retry.Jitter = func(delay time.Duration) time.Duration { return delay }
	}
	if options.Sleeper == nil {
		options.Sleeper = SleepFunc(func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		})
	}
	if options.MaxErrorBody <= 0 {
		options.MaxErrorBody = defaultMaxErrorBody
	}
	return &Client{
		metadata:     options.MetadataDoer,
		download:     options.DownloadDoer,
		userAgent:    options.UserAgent,
		retry:        options.Retry,
		sleeper:      options.Sleeper,
		maxErrorBody: options.MaxErrorBody,
	}
}

// Do performs a request. Retrying is only enabled for safe, idempotent reads.
func (c *Client) Do(ctx context.Context, request *http.Request, options RequestOptions) (*http.Response, error) {
	if request == nil {
		return nil, errors.New("HTTP request is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attempts := 1
	if options.Retry {
		attempts = c.retry.MaxAttempts
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, c.transportError(request, options, false, err)
		}
		current := request.Clone(ctx)
		if current.Header.Get("User-Agent") == "" {
			current.Header.Set("User-Agent", c.userAgent)
		}
		response, err := c.doer(options.Kind).Do(current)
		if err != nil {
			retryable := options.Retry && isRetryableTransport(err) && attempt < attempts
			if retryable {
				if err := c.sleepRetry(ctx, attempt, 0); err != nil {
					return nil, c.transportError(request, options, false, err)
				}
				continue
			}
			unknown := !options.Retry && isWriteMethod(request.Method)
			return nil, c.transportError(request, options, unknown, err)
		}
		if options.Retry && isRetryableStatus(response.StatusCode) && attempt < attempts {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
			drainAndClose(response.Body)
			if err := c.sleepRetry(ctx, attempt, retryAfter); err != nil {
				return nil, c.transportError(request, options, false, err)
			}
			continue
		}
		return response, nil
	}
	return nil, c.transportError(request, options, false, errors.New("retry attempts exhausted"))
}

// Read retries the complete lifecycle of an idempotent read, including an
// interrupted response-body read. Write requests must continue to use Do.
func (c *Client) Read(ctx context.Context, options ReadOptions, build RequestFactory, consume ResponseConsumer) error {
	if build == nil || consume == nil {
		return errors.New("read request factory and response consumer are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	attempts := 1
	if options.Retry {
		attempts = c.retry.MaxAttempts
	}
	requestOptions := options.RequestOptions
	requestOptions.Retry = false
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if options.BeforeAttempt != nil {
			if err := options.BeforeAttempt(attempt); err != nil {
				return err
			}
		}
		request, err := build(ctx)
		if err != nil {
			return err
		}
		response, err := c.Do(ctx, request, requestOptions)
		if err != nil {
			if attempt < attempts && retryableReadError(err) {
				if err := c.sleepRetry(ctx, attempt, 0); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if isRetryableStatus(response.StatusCode) && attempt < attempts {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now())
			drainAndClose(response.Body)
			if err := c.sleepRetry(ctx, attempt, retryAfter); err != nil {
				return err
			}
			continue
		}
		if err := c.CheckResponse(response, options.Operation); err != nil {
			return err
		}
		consumeErr := consume(response)
		closeErr := response.Body.Close()
		if consumeErr == nil && closeErr != nil {
			consumeErr = closeErr
		}
		if consumeErr == nil {
			return nil
		}
		if attempt < attempts && retryableBodyError(consumeErr) {
			if err := c.sleepRetry(ctx, attempt, 0); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("%s response body: %w", options.Operation, consumeErr)
	}
	return errors.New("read retry attempts exhausted")
}

// CheckResponse returns a redacted-safe error for non-2xx responses. The body
// is deliberately discarded: a remote API can echo credentials in an error.
func (c *Client) CheckResponse(response *http.Response, operation string) error {
	if response == nil {
		return errors.New("HTTP response is nil")
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	drainAndCloseLimit(response.Body, c.maxErrorBody)
	method, rawURL := "", ""
	if response.Request != nil {
		method = response.Request.Method
		rawURL = RedactURL(response.Request.URL)
	}
	summary := "remote API returned " + response.Status
	if response.Status == "" {
		summary = fmt.Sprintf("remote API returned status %d", response.StatusCode)
	}
	return &OpError{
		Operation:  operation,
		Method:     method,
		URL:        rawURL,
		Status:     response.StatusCode,
		Retryable:  isRetryableStatus(response.StatusCode),
		RetryAfter: parseRetryAfter(response.Header.Get("Retry-After"), time.Now()),
		Summary:    summary,
	}
}

// RedactURL returns only scheme, host, and path. Query parameters and fragments
// are omitted because signed browser-download URLs are secrets too.
func RedactURL(value *url.URL) string {
	if value == nil {
		return ""
	}
	copy := *value
	copy.RawQuery = ""
	copy.ForceQuery = false
	copy.Fragment = ""
	copy.RawFragment = ""
	return copy.String()
}

func (c *Client) doer(kind Kind) Doer {
	if kind == Download {
		return c.download
	}
	return c.metadata
}

func (c *Client) sleepRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	delay := retryAfter
	if delay <= 0 {
		delay = c.retry.BaseDelay
		for iteration := 1; iteration < attempt; iteration++ {
			delay *= 2
			if delay >= c.retry.MaxDelay {
				delay = c.retry.MaxDelay
				break
			}
		}
	}
	if delay > c.retry.MaxDelay {
		delay = c.retry.MaxDelay
	}
	return c.sleeper.Sleep(ctx, c.retry.Jitter(delay))
}

func (c *Client) transportError(request *http.Request, options RequestOptions, unknown bool, err error) error {
	summary := "transport error"
	if errors.Is(err, context.Canceled) {
		summary = "request canceled"
	} else if errors.Is(err, context.DeadlineExceeded) {
		summary = "request deadline exceeded"
	}
	return &OpError{
		Operation: options.Operation,
		Method:    request.Method,
		URL:       RedactURL(request.URL),
		Retryable: isRetryableTransport(err),
		Unknown:   unknown,
		Summary:   summary,
		Err:       err,
	}
}

func retryableReadError(err error) bool {
	var opErr *OpError
	if errors.As(err, &opErr) {
		return opErr.Retryable && !opErr.Unknown
	}
	return isRetryableTransport(err)
}

func retryableBodyError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, io.ErrUnexpectedEOF)
}

func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func isRetryableTransport(err error) bool {
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func isRetryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if timestamp, err := http.ParseTime(value); err == nil && timestamp.After(now) {
		return timestamp.Sub(now)
	}
	return 0
}

func drainAndClose(body io.ReadCloser) {
	drainAndCloseLimit(body, 4096)
}

func drainAndCloseLimit(body io.ReadCloser, limit int64) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, limit))
	_ = body.Close()
}
