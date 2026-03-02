package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// isRetryable
// ---------------------------------------------------------------------------

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusBadRequest, false},          // 400
		{http.StatusUnauthorized, false},        // 401
		{http.StatusForbidden, false},           // 403
		{http.StatusNotFound, false},            // 404
		{http.StatusTooManyRequests, true},       // 429
		{http.StatusInternalServerError, true},   // 500
		{http.StatusBadGateway, true},            // 502
		{http.StatusServiceUnavailable, true},    // 503
		{http.StatusGatewayTimeout, true},        // 504
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, isRetryable(tt.status), "status %d", tt.status)
	}
}

// ---------------------------------------------------------------------------
// parseRetryAfter
// ---------------------------------------------------------------------------

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"30", 30 * time.Second},
		{"1", 1 * time.Second},
		{"not-a-number", 0},
		{"12.5", 0}, // float not supported, returns 0
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, parseRetryAfter(tt.value), "value %q", tt.value)
	}
}

// ---------------------------------------------------------------------------
// NewProviderError
// ---------------------------------------------------------------------------

func TestNewProviderError_JSONBody(t *testing.T) {
	body := `{"error": {"message": "Rate limit exceeded", "type": "rate_limit_error"}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	err := NewProviderError("anthropic", resp)

	assert.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	assert.Equal(t, "anthropic", err.Provider)
	assert.True(t, err.Retryable)
	assert.Contains(t, err.Message, "Rate limit exceeded")
}

func TestNewProviderError_PlainTextBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("Service Unavailable")),
	}

	err := NewProviderError("google", resp)

	assert.Equal(t, http.StatusServiceUnavailable, err.StatusCode)
	assert.Equal(t, "google", err.Provider)
	assert.True(t, err.Retryable)
	assert.Equal(t, "Service Unavailable", err.Message)
}

func TestNewProviderError_RetryAfterHeader(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"15"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
	}

	err := NewProviderError("google", resp)

	assert.Equal(t, 15*time.Second, err.RetryAfter)
}

func TestNewProviderError_NonRetryable(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error": "invalid api key"}`)),
	}

	err := NewProviderError("anthropic", resp)

	assert.False(t, err.Retryable)
	assert.Equal(t, http.StatusUnauthorized, err.StatusCode)
}

func TestProviderError_ErrorString(t *testing.T) {
	err := &ProviderError{
		StatusCode: 429,
		Provider:   "google",
		Message:    "rate limited",
	}
	assert.Equal(t, "google API error (status 429): rate limited", err.Error())
}

// ---------------------------------------------------------------------------
// Retry
// ---------------------------------------------------------------------------

func TestRetry_ImmediateSuccess(t *testing.T) {
	calls := 0
	err := Retry(context.Background(), 3, func() error {
		calls++
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, 1, calls)
}

func TestRetry_NonRetryableError(t *testing.T) {
	// A 401 should fail immediately without retrying.
	calls := 0
	err := Retry(context.Background(), 3, func() error {
		calls++
		return &ProviderError{
			StatusCode: http.StatusUnauthorized,
			Provider:   "anthropic",
			Message:    "invalid api key",
			Retryable:  false,
		}
	})

	require.Error(t, err)
	assert.Equal(t, 1, calls, "should not retry non-retryable errors")

	var provErr *ProviderError
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, http.StatusUnauthorized, provErr.StatusCode)
}

func TestRetry_NonProviderError(t *testing.T) {
	// A plain error (not ProviderError) should not be retried.
	calls := 0
	err := Retry(context.Background(), 3, func() error {
		calls++
		return fmt.Errorf("network timeout")
	})

	require.Error(t, err)
	assert.Equal(t, 1, calls, "should not retry non-ProviderError")
}

func TestRetry_SucceedsOnSecondAttempt(t *testing.T) {
	// First call returns 429, second call succeeds.
	// This test takes ~1s due to one backoff sleep.
	calls := 0
	var result string
	err := Retry(context.Background(), 3, func() error {
		calls++
		if calls == 1 {
			return &ProviderError{
				StatusCode: http.StatusTooManyRequests,
				Provider:   "google",
				Message:    "rate limited",
				Retryable:  true,
			}
		}
		result = "success"
		return nil
	})

	assert.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, "success", result, "closure should capture result")
}

func TestRetry_ExhaustsAttempts(t *testing.T) {
	// All attempts fail with retryable error. Use maxAttempts=2
	// so we only wait through one backoff.
	calls := 0
	err := Retry(context.Background(), 2, func() error {
		calls++
		return &ProviderError{
			StatusCode: http.StatusTooManyRequests,
			Provider:   "google",
			Message:    "rate limited",
			Retryable:  true,
		}
	})

	require.Error(t, err)
	assert.Equal(t, 2, calls, "should attempt exactly maxAttempts times")

	var provErr *ProviderError
	require.ErrorAs(t, err, &provErr)
	assert.Equal(t, http.StatusTooManyRequests, provErr.StatusCode)
}

func TestRetry_RespectsContextCancellation(t *testing.T) {
	// Cancel the context before Retry can sleep through the backoff.
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	err := Retry(ctx, 3, func() error {
		calls++
		// Cancel after the first call so the backoff select picks up ctx.Done().
		cancel()
		return &ProviderError{
			StatusCode: http.StatusTooManyRequests,
			Provider:   "google",
			Message:    "rate limited",
			Retryable:  true,
		}
	})

	require.Error(t, err)
	assert.Equal(t, 1, calls, "should stop after context cancellation")
	assert.ErrorIs(t, err, context.Canceled)
}

// ---------------------------------------------------------------------------
// backoffDelay
// ---------------------------------------------------------------------------

func TestBackoffDelay_ExponentialGrowth(t *testing.T) {
	for attempt := 0; attempt < 3; attempt++ {
		delay := backoffDelay(attempt, 0)
		// Base is 2^attempt seconds. Jitter adds 0–500ms.
		minExpected := time.Second * time.Duration(1<<attempt) // 1s, 2s, 4s
		maxExpected := minExpected + 500*time.Millisecond

		assert.GreaterOrEqual(t, delay, minExpected, "attempt %d", attempt)
		assert.LessOrEqual(t, delay, maxExpected, "attempt %d", attempt)
	}
}

func TestBackoffDelay_RespectsRetryAfterFloor(t *testing.T) {
	// RetryAfter of 10s should override the computed delay for attempt 0
	// (which would be ~1–1.5s).
	delay := backoffDelay(0, 10*time.Second)
	assert.GreaterOrEqual(t, delay, 10*time.Second)
}
