package provider

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// anthropicBaseURL is the Anthropic API base URL used in cassettes.
const anthropicBaseURL = "https://api.anthropic.com/v1"

// newAnthropicTestProvider creates an AnthropicProvider wired to a go-vcr
// replay client.
func newAnthropicTestProvider(t *testing.T, cassette string) *AnthropicProvider {
	t.Helper()
	client := newReplayClient(t, cassette)
	return NewAnthropicProvider("fake-api-key", anthropicBaseURL, client)
}

// simpleAnthropicRequest returns a minimal ChatRequest for Anthropic tests.
func simpleAnthropicRequest(content string) *ChatRequest {
	return &ChatRequest{
		Model:    "claude-haiku-4-5-20251001",
		Messages: []Message{{Role: "user", Content: content}},
	}
}

// ---------------------------------------------------------------------------
// Non-streaming
// ---------------------------------------------------------------------------

func TestAnthropicChatCompletion(t *testing.T) {
	p := newAnthropicTestProvider(t, "anthropic_chat_completion")

	resp, err := p.ChatCompletion(context.Background(), simpleAnthropicRequest("What is the capital of France?"))

	require.NoError(t, err)

	// Anthropic returns a response ID (Gemini doesn't), so we verify
	// it's passed through to our unified response.
	assert.Equal(t, "msg_01XFDUDYJgAACzvnptvVoYEL", resp.ID)
	assert.Equal(t, "claude-haiku-4-5-20251001", resp.Model)
	assert.Equal(t, "The capital of France is Paris.", resp.Content)

	// Anthropic uses input_tokens/output_tokens (not promptTokenCount).
	// The adapter translates these AND computes TotalTokens (which
	// Anthropic doesn't return — the adapter adds them).
	assert.Equal(t, 15, resp.Usage.PromptTokens)
	assert.Equal(t, 9, resp.Usage.CompletionTokens)
	assert.Equal(t, 24, resp.Usage.TotalTokens) // 15 + 9, computed by adapter
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

func TestAnthropicChatCompletionStream(t *testing.T) {
	p := newAnthropicTestProvider(t, "anthropic_chat_completion_stream")

	req := simpleAnthropicRequest("What is the capital of France?")
	req.Stream = true

	ch, err := p.ChatCompletionStream(context.Background(), req)
	require.NoError(t, err)

	// Collect all chunks. The cassette SSE body contains:
	//   message_start      → adapter grabs ID, model, input_tokens
	//   content_block_start → adapter skips (default case in switch)
	//   ping               → adapter skips (default case)
	//   content_block_delta → "The capital" (emitted as StreamChunk)
	//   content_block_delta → " of France is Paris." (emitted)
	//   content_block_stop  → adapter skips
	//   message_delta       → adapter grabs output_tokens
	//   message_stop        → adapter emits final Done chunk with Usage
	//
	// So we expect 3 chunks on the channel:
	//   1. "The capital" (intermediate)
	//   2. " of France is Paris." (intermediate)
	//   3. "" (final Done chunk with Usage)
	var chunks []StreamChunk
	for chunk := range ch {
		require.NoError(t, chunk.Error)
		chunks = append(chunks, chunk)
	}

	require.Len(t, chunks, 3)

	// First content chunk: has text, not done, carries the response
	// ID and model (set from message_start event).
	assert.Equal(t, "The capital", chunks[0].Delta)
	assert.False(t, chunks[0].Done)
	assert.Equal(t, "msg_01XFDUDYJgAACzvnptvVoYEL", chunks[0].ID)
	assert.Equal(t, "claude-haiku-4-5-20251001", chunks[0].Model)

	// Second content chunk.
	assert.Equal(t, " of France is Paris.", chunks[1].Delta)
	assert.False(t, chunks[1].Done)

	// Final chunk (from message_stop): Done=true, no text delta,
	// Usage accumulated from message_start (input) + message_delta (output).
	assert.Equal(t, "", chunks[2].Delta)
	assert.True(t, chunks[2].Done)
	require.NotNil(t, chunks[2].Usage)
	assert.Equal(t, 15, chunks[2].Usage.PromptTokens)
	assert.Equal(t, 9, chunks[2].Usage.CompletionTokens)
	assert.Equal(t, 24, chunks[2].Usage.TotalTokens) // computed by adapter

	// Verify full text from stream.
	combined := chunks[0].Delta + chunks[1].Delta
	assert.Equal(t, "The capital of France is Paris.", combined)
}

// ---------------------------------------------------------------------------
// HTTP errors (table-driven)
// ---------------------------------------------------------------------------

func TestAnthropicChatCompletion_HTTPErrors(t *testing.T) {
	tests := []struct {
		name       string
		cassette   string
		wantStatus int
		wantRetry  bool
	}{
		{
			name:       "rate_limit_429",
			cassette:   "anthropic_error_429",
			wantStatus: 429,
			wantRetry:  true,
		},
		{
			name:       "unauthorized_401",
			cassette:   "anthropic_error_401",
			wantStatus: 401,
			wantRetry:  false,
		},
		{
			name:       "server_error_500",
			cassette:   "anthropic_error_500",
			wantStatus: 500,
			wantRetry:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newAnthropicTestProvider(t, tt.cassette)

			_, err := p.ChatCompletion(context.Background(), simpleAnthropicRequest("Hello"))

			require.Error(t, err)
			var provErr *ProviderError
			require.ErrorAs(t, err, &provErr)
			assert.Equal(t, tt.wantStatus, provErr.StatusCode)
			assert.Equal(t, "anthropic", provErr.Provider)
			assert.Equal(t, tt.wantRetry, provErr.Retryable)
		})
	}
}
