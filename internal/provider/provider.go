// Package provider defines the Provider interface and LLM provider adapters.
//
// Every LLM backend (Google, Anthropic, etc.) implements the Provider
// interface. The rest of the gateway works with these unified types —
// handlers, cache, router — so they never need to know which provider
// is actually handling a request.
package provider

import "context"

// Provider is the interface that every LLM backend must satisfy.
// Go interfaces are implicit: any struct that has these three methods
// automatically implements Provider — no "implements" keyword needed.
type Provider interface {
	// Name returns the provider identifier, e.g. "google" or "anthropic".
	// Used for logging, metrics labels, and the X-LLMRouter-Provider header.
	Name() string

	// ChatCompletion sends a request and returns the complete response.
	// This is the non-streaming path (when the client sends stream: false).
	//
	// The context.Context parameter carries cancellation signals and
	// deadlines. If the client disconnects, ctx gets cancelled, and the
	// provider adapter should stop waiting for the upstream API.
	ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)

	// ChatCompletionStream sends a request and returns a channel that
	// delivers response chunks as they arrive from the upstream API.
	//
	// The returned channel is receive-only (<-chan) — the caller can read
	// from it but not write to it. The adapter creates the channel
	// internally, writes chunks to it, and closes it when the stream ends.
	//
	// Think of it like an async generator in JS:
	//   async function* stream(req) { yield chunk1; yield chunk2; }
	// except in Go you read from a channel instead of using for-await-of.
	ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error)
}

// ---------------------------------------------------------------------------
// Unified request types
// ---------------------------------------------------------------------------

// ChatRequest is the internal representation of a chat completion request.
// The HTTP handler parses the incoming OpenAI-format JSON into this struct,
// and provider adapters translate it into their backend-specific format.
type ChatRequest struct {
	Model     string    `json:"model"`      // e.g. "gemini-2.0-flash", "auto"
	Messages  []Message `json:"messages"`   // the conversation history
	Stream    bool      `json:"stream"`     // true = SSE streaming
	MaxTokens int       `json:"max_tokens"` // max tokens in the response
}

// Message is a single message in the conversation. This matches the OpenAI
// format, which uses role + content pairs. Google and Anthropic use different
// structures (Google has "parts", Anthropic separates "system"), so each
// adapter translates from this common format.
type Message struct {
	Role    string `json:"role"`    // "system", "user", or "assistant"
	Content string `json:"content"` // the message text
}

// ---------------------------------------------------------------------------
// Unified response types
// ---------------------------------------------------------------------------

// ChatResponse is the internal representation of a complete (non-streaming)
// chat completion response. Provider adapters translate their backend's
// response format into this struct, and the handler serializes it as
// OpenAI-format JSON back to the client.
type ChatResponse struct {
	ID      string // unique response ID from the provider
	Model   string // the model that actually generated the response
	Content string // the generated text
	Usage   Usage  // token counts for cost tracking and metrics
}

// Usage holds token count information. Every provider returns this in some
// form — we normalize it here. These numbers feed into cost calculation
// (tokens × price-per-token) and Prometheus metrics.
type Usage struct {
	PromptTokens     int // tokens in the input (our request)
	CompletionTokens int // tokens in the output (model's response)
	TotalTokens      int // sum of the above
}

// StreamChunk is one piece of a streaming response. The provider adapter
// sends these over a channel, and the SSE writer (stream package) reads
// them and flushes each one to the client as a server-sent event.
type StreamChunk struct {
	ID    string // response ID (same value across all chunks in one stream)
	Model string // model name
	Delta string // the new text fragment in this chunk
	Done  bool   // true on the final chunk — signals the stream is complete

	// Usage is only populated on the final chunk (some providers include
	// token counts at the end of a stream). It's a pointer so it can be
	// nil on all non-final chunks — like TypeScript's `usage?: Usage`.
	Usage *Usage
}
