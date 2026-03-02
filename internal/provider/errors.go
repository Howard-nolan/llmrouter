package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"time"
)

// ProviderError is a structured error returned when an upstream LLM provider
// responds with a non-2xx HTTP status. It carries enough context for the
// handler to decide what HTTP status to send back to the client and whether
// the request should be retried.
//
// In Go, any type with an Error() string method satisfies the built-in
// `error` interface — like implementing toString() on a custom Error
// subclass in Node.js. This means ProviderError can be returned anywhere
// a plain `error` is expected, but callers can "unwrap" it with
// errors.As() to access the structured fields.
type ProviderError struct {
	// StatusCode is the HTTP status from the upstream provider (e.g. 429, 401, 500).
	StatusCode int

	// Provider identifies which provider returned the error ("google", "anthropic").
	Provider string

	// Message is the human-readable error detail from the provider's response body.
	Message string

	// Retryable indicates whether this error is worth retrying. True for
	// transient failures (429 rate limit, 5xx server errors), false for
	// permanent failures (401 bad key, 400 bad request).
	Retryable bool

	// RetryAfter is the duration the provider asked us to wait before
	// retrying, parsed from the Retry-After header. Zero if not present.
	RetryAfter time.Duration
}

// Error satisfies the built-in error interface. This is the method that
// makes ProviderError usable anywhere a plain `error` is expected.
//
// When you do fmt.Println(err) or log.Printf("%v", err), Go calls this
// method to get the string representation. It's the equivalent of
// Error.prototype.toString() in JavaScript.
func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s API error (status %d): %s", e.Provider, e.StatusCode, e.Message)
}

// NewProviderError creates a ProviderError from an HTTP response.
// It reads the response body to extract the error message, and classifies
// whether the error is retryable based on the status code.
//
// This replaces the duplicated error-reading pattern that was in both
// google.go and anthropic.go — each had its own:
//
//	var errBody map[string]any
//	json.NewDecoder(httpResp.Body).Decode(&errBody)
//	return fmt.Errorf("... API error (status %d): %v", status, errBody)
//
// Now both adapters call NewProviderError(name, resp) instead.
//
// IMPORTANT: This function reads from httpResp.Body but does NOT close it.
// The caller is responsible for closing the body (typically via defer).
// This follows Go convention — the creator of the resource manages its
// lifecycle. It's like how in Node.js, the function that opens a stream
// is usually responsible for closing it.
func NewProviderError(providerName string, httpResp *http.Response) *ProviderError {
	// Read the response body to get the error details.
	// io.ReadAll reads the entire body into a byte slice. We use this
	// instead of json.NewDecoder because the body might not be valid
	// JSON (some providers return plain text errors on certain failures).
	//
	// We cap the read at a reasonable size to avoid allocating huge
	// amounts of memory if the provider sends back something unexpected.
	// io.LimitReader wraps the reader with a byte limit — like
	// stream.pipeline with a Transform that stops after N bytes.
	bodyBytes, err := io.ReadAll(io.LimitReader(httpResp.Body, 4096))

	// Default message if we can't read the body.
	message := "unknown error"

	if err == nil && len(bodyBytes) > 0 {
		// Try to parse as JSON for a cleaner message.
		// Many provider error responses look like:
		//   {"error": {"message": "Rate limit exceeded", "type": "rate_limit_error"}}
		//
		// We try to extract a useful string from the JSON. If it's not
		// valid JSON, we fall back to the raw body text.
		var parsed map[string]any
		if json.Unmarshal(bodyBytes, &parsed) == nil {
			message = fmt.Sprintf("%v", parsed)
		} else {
			message = string(bodyBytes)
		}
	}

	return &ProviderError{
		StatusCode: httpResp.StatusCode,
		Provider:   providerName,
		Message:    message,
		Retryable:  isRetryable(httpResp.StatusCode),
		RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After")),
	}
}

// isRetryable determines whether an HTTP status code represents a transient
// failure that's worth retrying.
//
//   - 429 (Too Many Requests): Rate limited. The provider wants us to slow
//     down. Retrying after a backoff usually succeeds.
//   - 5xx (Server Errors): The provider's servers are having trouble.
//     These are often transient — a retry a few seconds later may work.
//   - Everything else (400, 401, 403, 404, etc.): These are client errors
//     or permanent failures. Retrying won't help — the request itself is
//     wrong, or the credentials are invalid.
// parseRetryAfter parses the Retry-After header value into a duration.
// The header can be either seconds ("30") or an HTTP-date, but providers
// almost always use seconds. Returns zero if empty or unparseable.
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func isRetryable(statusCode int) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode >= 500 {
		return true
	}
	return false
}

// Retry calls fn up to maxAttempts times, retrying only when the error is a
// retryable ProviderError. Uses exponential backoff with jitter between
// attempts. Respects Retry-After headers and context cancellation.
//
// The caller uses a closure to capture return values beyond the error:
//
//	var resp *ChatResponse
//	err := Retry(ctx, 3, func() error {
//	    var callErr error
//	    resp, callErr = p.ChatCompletion(ctx, &req)
//	    return callErr
//	})
func Retry(ctx context.Context, maxAttempts int, fn func() error) error {
	var lastErr error

	for attempt := range maxAttempts {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		// Check if the error is a retryable ProviderError.
		var provErr *ProviderError
		if !errors.As(lastErr, &provErr) || !provErr.Retryable {
			return lastErr
		}

		// Don't sleep after the last attempt.
		if attempt == maxAttempts-1 {
			break
		}

		delay := backoffDelay(attempt, provErr.RetryAfter)

		// Wait for the delay or context cancellation, whichever comes first.
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return lastErr
}

// backoffDelay computes the wait duration for a retry attempt.
// Uses exponential backoff (1s, 2s, 4s, ...) with random jitter (0–500ms)
// to prevent thundering herd. If the provider sent a Retry-After header,
// that value is used as a floor.
func backoffDelay(attempt int, retryAfter time.Duration) time.Duration {
	// Exponential: 1s * 2^attempt
	base := time.Second * time.Duration(math.Pow(2, float64(attempt)))

	// Jitter: add 0–500ms of randomness.
	jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))

	delay := base + jitter

	// If the provider told us how long to wait, use that as a floor.
	if retryAfter > delay {
		delay = retryAfter
	}

	return delay
}
