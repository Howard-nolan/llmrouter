package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// googleBaseURL is the Gemini API base URL used in cassettes.
// We define it once to avoid typos across tests.
const googleBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// newGoogleTestProvider creates a GoogleProvider wired to a go-vcr replay
// client. Every test that exercises the Google adapter uses this helper.
func newGoogleTestProvider(t *testing.T, cassette string) *GoogleProvider {
	t.Helper()
	client := newReplayClient(t, cassette)
	return NewGoogleProvider("fake-api-key", googleBaseURL, client)
}

// simpleGoogleRequest returns a minimal ChatRequest for testing.
// Using a helper avoids repeating the same struct literal in every test.
func simpleGoogleRequest(content string) *ChatRequest {
	return &ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: content}},
	}
}

// ---------------------------------------------------------------------------
// Non-streaming
// ---------------------------------------------------------------------------

func TestGoogleChatCompletion(t *testing.T) {
	// Create a provider backed by the recorded cassette. When
	// p.ChatCompletion makes an HTTP POST, go-vcr intercepts it
	// and returns the stored response from the YAML file.
	p := newGoogleTestProvider(t, "google_chat_completion")

	resp, err := p.ChatCompletion(context.Background(), simpleGoogleRequest("What is the capital of France?"))

	// require.NoError stops the test immediately if err != nil.
	// We use require (not assert) because the remaining assertions
	// all dereference resp — if err != nil, resp is nil and we'd
	// get a nil pointer panic instead of a clear failure message.
	require.NoError(t, err)

	// These values come from the cassette. The test verifies that
	// the adapter correctly translates Gemini's response shape
	// (candidates[0].content.parts[0].text, usageMetadata) into
	// our unified ChatResponse struct.
	assert.Equal(t, "gemini-2.0-flash", resp.Model)
	assert.Equal(t, "The capital of France is Paris.", resp.Content)
	assert.Equal(t, 10, resp.Usage.PromptTokens)
	assert.Equal(t, 8, resp.Usage.CompletionTokens)
	assert.Equal(t, 18, resp.Usage.TotalTokens)
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

func TestGoogleChatCompletionStream(t *testing.T) {
	p := newGoogleTestProvider(t, "google_chat_completion_stream")

	req := simpleGoogleRequest("What is the capital of France?")
	req.Stream = true

	// ChatCompletionStream returns a receive-only channel (<-chan StreamChunk).
	// The adapter spawns a goroutine that reads SSE events from the
	// (replayed) response body and sends StreamChunks on this channel.
	ch, err := p.ChatCompletionStream(context.Background(), req)
	require.NoError(t, err)

	// Collect all chunks from the channel. The for-range loop reads
	// until the channel is closed (which the goroutine does via
	// defer close(ch) when it finishes reading the response body).
	//
	// In Node.js terms, this is like:
	//   const chunks = [];
	//   for await (const chunk of stream) { chunks.push(chunk); }
	var chunks []StreamChunk
	for chunk := range ch {
		require.NoError(t, chunk.Error)
		chunks = append(chunks, chunk)
	}

	// We expect 2 chunks from the cassette:
	//   1. "The capital" (intermediate, Done=false)
	//   2. " of France is Paris." (final, Done=true, has Usage)
	require.Len(t, chunks, 2)

	// Intermediate chunk: has text but no Done flag or Usage.
	assert.Equal(t, "The capital", chunks[0].Delta)
	assert.False(t, chunks[0].Done)
	assert.Nil(t, chunks[0].Usage)

	// Final chunk: has text, Done=true, and Usage from the cassette's
	// usageMetadata field. This verifies the adapter correctly handles
	// the edge case where Gemini sends content AND finishReason in the
	// same SSE event (the adapter emits one chunk with both).
	assert.Equal(t, " of France is Paris.", chunks[1].Delta)
	assert.True(t, chunks[1].Done)
	require.NotNil(t, chunks[1].Usage)
	assert.Equal(t, 10, chunks[1].Usage.PromptTokens)
	assert.Equal(t, 8, chunks[1].Usage.CompletionTokens)
	assert.Equal(t, 18, chunks[1].Usage.TotalTokens)

	// Verify the full text by concatenating all deltas.
	combined := chunks[0].Delta + chunks[1].Delta
	assert.Equal(t, "The capital of France is Paris.", combined)
}

// ---------------------------------------------------------------------------
// HTTP errors (table-driven)
// ---------------------------------------------------------------------------

func TestGoogleChatCompletion_HTTPErrors(t *testing.T) {
	// Table-driven tests: each entry is a test case with a name,
	// a cassette file, and the expected ProviderError fields.
	//
	// This pattern is idiomatic Go — it keeps related tests grouped
	// and avoids duplicating the same assertion logic. In Jest terms,
	// it's like test.each([...]).
	tests := []struct {
		name       string
		cassette   string
		wantStatus int
		wantRetry  bool
	}{
		{
			name:       "rate_limit_429",
			cassette:   "google_error_429",
			wantStatus: 429,
			wantRetry:  true, // 429 is transient — worth retrying
		},
		{
			name:       "unauthorized_401",
			cassette:   "google_error_401",
			wantStatus: 401,
			wantRetry:  false, // bad credentials won't fix themselves
		},
		{
			name:       "server_error_500",
			cassette:   "google_error_500",
			wantStatus: 500,
			wantRetry:  true, // server errors are often transient
		},
	}

	for _, tt := range tests {
		// t.Run creates a subtest. Each row in the table gets its own
		// test with its own pass/fail status. In the output you'll see:
		//   TestGoogleChatCompletion_HTTPErrors/rate_limit_429
		//   TestGoogleChatCompletion_HTTPErrors/unauthorized_401
		//   etc.
		t.Run(tt.name, func(t *testing.T) {
			p := newGoogleTestProvider(t, tt.cassette)

			_, err := p.ChatCompletion(context.Background(), simpleGoogleRequest("Hello"))

			// The adapter should return a *ProviderError.
			// errors.As unwraps the error chain looking for a
			// *ProviderError — like err instanceof ProviderError in JS,
			// but it also traverses wrapped errors (from fmt.Errorf %w).
			require.Error(t, err)
			var provErr *ProviderError
			require.ErrorAs(t, err, &provErr)
			assert.Equal(t, tt.wantStatus, provErr.StatusCode)
			assert.Equal(t, "google", provErr.Provider)
			assert.Equal(t, tt.wantRetry, provErr.Retryable)
		})
	}
}

// ---------------------------------------------------------------------------
// Malformed response
// ---------------------------------------------------------------------------

func TestGoogleChatCompletion_MalformedResponse(t *testing.T) {
	// The cassette returns HTTP 200 with body "this is not valid json".
	// The adapter will pass the status check (200 == OK) but fail when
	// trying to json.Decode the body. This should produce a plain error,
	// NOT a *ProviderError (since the HTTP status was fine — it's the
	// body that's broken).
	p := newGoogleTestProvider(t, "google_malformed_response")

	_, err := p.ChatCompletion(context.Background(), simpleGoogleRequest("Hello"))

	require.Error(t, err)

	// Verify it's NOT a ProviderError. errors.As returns false if no
	// *ProviderError exists in the error chain.
	var provErr *ProviderError
	assert.False(t, errors.As(err, &provErr), "malformed response should not produce a ProviderError")

	// The error message should mention the decoding failure.
	assert.Contains(t, err.Error(), "decoding gemini response")
}
