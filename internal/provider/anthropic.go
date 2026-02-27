package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// AnthropicProvider struct + constructor
// ---------------------------------------------------------------------------

// AnthropicProvider implements the Provider interface for Anthropic's
// Messages API. Same pattern as GoogleProvider: translate our unified
// ChatRequest into Anthropic's format, make the HTTP call, translate back.
type AnthropicProvider struct {
	apiKey  string
	baseURL string       // e.g. "https://api.anthropic.com/v1"
	client  *http.Client
}

// NewAnthropicProvider creates an AnthropicProvider ready to make API calls.
func NewAnthropicProvider(apiKey, baseURL string, client *http.Client) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey:  apiKey,
		baseURL: baseURL,
		client:  client,
	}
}

// Name returns the provider identifier.
func (a *AnthropicProvider) Name() string {
	return "anthropic"
}

// ---------------------------------------------------------------------------
// Anthropic API types (unexported)
// ---------------------------------------------------------------------------

// --- Request types ---

// anthropicRequest is the top-level request body for Anthropic's
// /v1/messages endpoint.
//
// Key differences from Gemini:
//   - "system" is a top-level string, not nested inside messages
//   - "max_tokens" is REQUIRED (Anthropic rejects requests without it)
//   - "model" is in the request body (Gemini puts it in the URL path)
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Stream    bool               `json:"stream,omitempty"`
}

// anthropicMessage is one message in the conversation.
// Unlike Gemini's nested parts structure, Anthropic uses a flat
// role + content shape — same as OpenAI's format.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// --- Response types ---

// anthropicResponse is the top-level response from Anthropic's /v1/messages.
//
// Key differences from Gemini's response:
//   - "content" is an array of content blocks (not candidates[0].content.parts)
//   - "usage" uses input_tokens/output_tokens (not promptTokenCount/candidatesTokenCount)
//   - "id" is returned at the top level (Gemini doesn't return a response ID)
//   - "stop_reason" instead of "finishReason"
type anthropicResponse struct {
	ID         string                  `json:"id"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// anthropicContentBlock is one piece of the response. Anthropic returns an
// array because responses can mix text and tool_use blocks. For our purposes,
// we only care about blocks where type == "text".
type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// anthropicUsage holds token counts. Note the different JSON field names
// from Gemini — each provider names these slightly differently, which is
// exactly why we have our unified Usage type.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// --- Streaming event types ---
//
// Anthropic's streaming format is more complex than Gemini's. Gemini sends
// the same JSON shape for every SSE event — you just parse data: lines.
// Anthropic sends NAMED events, each with a different JSON payload shape:
//
//   event: message_start      → contains response ID, model, input token count
//   event: content_block_delta → contains a text fragment (the actual tokens)
//   event: message_delta      → contains stop_reason and output token count
//   event: message_stop       → signals the stream is done (empty payload)
//
// We need different structs for each payload shape. Every payload includes
// a "type" field that matches the event name, so we can decode into a
// generic wrapper first, check the type, then decode the specific fields.

// anthropicStreamEvent is a lightweight wrapper for initial decoding.
// We unmarshal into this first just to read the "type" field, then
// decide how to handle the rest of the fields based on that type.
//
// Think of it like a discriminated union in TypeScript:
//   type Event = { type: "message_start", message: {...} }
//               | { type: "content_block_delta", delta: {...} }
//               | ...
// except Go doesn't have union types, so we put all possible fields
// in one struct and leave the irrelevant ones empty (zero-valued).
type anthropicStreamEvent struct {
	Type    string                `json:"type"`
	Message *anthropicEventMessage `json:"message,omitempty"` // present on message_start
	Delta   *anthropicEventDelta  `json:"delta,omitempty"`   // present on content_block_delta AND message_delta
	Usage   *anthropicUsage       `json:"usage,omitempty"`   // present on message_delta (output tokens)
}

// anthropicEventMessage is the "message" object inside a message_start event.
// It carries the response metadata: ID, model, and the input token count.
// Output tokens are 0 here because the model hasn't generated anything yet.
type anthropicEventMessage struct {
	ID    string         `json:"id"`
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"` // input_tokens populated, output_tokens = 0
}

// anthropicEventDelta carries different data depending on the event type:
//   - On content_block_delta: Type="text_delta", Text="the token text"
//   - On message_delta:       Type="", StopReason="end_turn" (text is empty)
//
// We put both fields in one struct because Go's zero values handle the
// "missing field" case naturally — an empty string means "not present."
type anthropicEventDelta struct {
	Type       string `json:"type,omitempty"`
	Text       string `json:"text,omitempty"`        // the text token (content_block_delta only)
	StopReason string `json:"stop_reason,omitempty"` // why the stream ended (message_delta only)
}

// anthropicAPIVersion pins the Anthropic API behavior. Anthropic requires
// this header on every request. It's how they version their API — instead
// of versioning the URL path (like /v2/messages), they use a date-based
// header. This lets them evolve the API without breaking older clients
// that send an older version string.
const anthropicAPIVersion = "2023-06-01"

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

// defaultMaxTokens is used when the caller doesn't specify max_tokens.
// Anthropic requires this field, so we need a fallback.
const defaultMaxTokens = 1024

// toAnthropicRequest translates our unified ChatRequest into Anthropic's
// format. Three things happen:
//  1. System messages get pulled out into the top-level "system" string
//  2. Remaining messages map directly (roles are already compatible)
//  3. max_tokens gets a default if not set (Anthropic requires it)
func toAnthropicRequest(req *ChatRequest) *anthropicRequest {
	ar := &anthropicRequest{
		Model: req.Model,
	}

	// Walk through messages and separate system messages from the rest.
	// Same idea as toGeminiRequest, but simpler: Anthropic wants a plain
	// string for system, not a structured object.
	var systemParts []string

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemParts = append(systemParts, msg.Content)
			continue
		}

		// No role mapping needed — Anthropic uses "user" and "assistant"
		// just like our unified format (unlike Gemini which uses "model").
		ar.Messages = append(ar.Messages, anthropicMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	// Join multiple system messages with newlines into one string.
	if len(systemParts) > 0 {
		ar.System = strings.Join(systemParts, "\n")
	}

	// Set max_tokens — use the caller's value if provided, otherwise default.
	if req.MaxTokens > 0 {
		ar.MaxTokens = req.MaxTokens
	} else {
		ar.MaxTokens = defaultMaxTokens
	}

	return ar
}

// ---------------------------------------------------------------------------
// Non-streaming: ChatCompletion
// ---------------------------------------------------------------------------

// ChatCompletion sends a non-streaming request to Anthropic's /v1/messages
// endpoint and returns the complete response.
//
// Same five-step flow as GoogleProvider.ChatCompletion:
//   translate → serialize → HTTP POST → decode response → translate back
//
// The main differences are in Step 3 (auth headers instead of query param)
// and Step 5 (different response shape to translate from).
func (a *AnthropicProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	// Step 1: Translate our unified request into Anthropic's format.
	anthropicReq := toAnthropicRequest(req)

	// Step 2: Serialize to JSON.
	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Step 3: Build the HTTP request.
	//
	// Anthropic's URL is simpler than Gemini's — the model is in the
	// request body (already set by toAnthropicRequest), not in the URL
	// path. So the endpoint is just {baseURL}/messages.
	//
	// Auth is different too: Gemini puts the API key in a query param
	// (?key=...), but Anthropic uses a custom header (x-api-key).
	// Most APIs use "Authorization: Bearer <token>", but Anthropic
	// chose their own header name — you just have to know this from
	// their docs.
	url := fmt.Sprintf("%s/messages", a.baseURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	// Step 4: Make the HTTP call and check for errors.
	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to anthropic: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		var errBody map[string]any
		json.NewDecoder(httpResp.Body).Decode(&errBody)
		return nil, fmt.Errorf("anthropic API error (status %d): %v",
			httpResp.StatusCode, errBody,
		)
	}

	// Step 5: Decode the JSON response.
	var anthropicResp anthropicResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&anthropicResp); err != nil {
		return nil, fmt.Errorf("decoding anthropic response: %w", err)
	}

	// Step 6: Translate back to our unified format.
	//
	// Anthropic returns content as an array of blocks. We need to find
	// the first text block. In practice, for a simple chat completion
	// (no tool use), content[0] is always type "text" — but we loop
	// to be safe, in case Anthropic ever reorders them or adds other
	// block types.
	var text string
	for _, block := range anthropicResp.Content {
		if block.Type == "text" {
			text = block.Text
			break
		}
	}

	resp := &ChatResponse{
		ID:      anthropicResp.ID,
		Model:   anthropicResp.Model,
		Content: text,
		Usage: Usage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		},
	}

	return resp, nil
}

// ---------------------------------------------------------------------------
// Streaming: ChatCompletionStream
// ---------------------------------------------------------------------------

// ChatCompletionStream sends a streaming request to Anthropic's /v1/messages
// endpoint and returns a channel of StreamChunks.
//
// The overall pattern is the same as Google's: HTTP POST → goroutine reads
// SSE lines → sends StreamChunks on channel. But the SSE parsing is more
// complex because Anthropic uses multiple named event types, each carrying
// a different JSON shape.
//
// The goroutine accumulates metadata across events:
//   - message_start    → grab response ID, model, input token count
//   - content_block_delta → extract text token, send as StreamChunk
//   - message_delta    → grab stop_reason and output token count
//   - message_stop     → final signal, send Done chunk with usage
func (a *AnthropicProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	// Step 1: Translate and serialize (same as non-streaming, but set
	// stream: true so Anthropic knows to return SSE).
	anthropicReq := toAnthropicRequest(req)
	anthropicReq.Stream = true

	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Step 2: Build the HTTP request.
	// Same endpoint as non-streaming — the "stream": true in the body
	// tells Anthropic to switch to SSE mode. This is different from
	// Gemini, where streaming uses a completely different URL path
	// (streamGenerateContent vs generateContent).
	url := fmt.Sprintf("%s/messages", a.baseURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicAPIVersion)

	// Step 3: Make the HTTP call.
	// Same as Google's streaming: do NOT defer Body.Close() here —
	// the goroutine owns the body and will close it when done.
	httpResp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to anthropic: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		var errBody map[string]any
		json.NewDecoder(httpResp.Body).Decode(&errBody)
		return nil, fmt.Errorf("anthropic API error (status %d): %v",
			httpResp.StatusCode, errBody,
		)
	}

	// Step 4: Create channel and launch the goroutine.
	ch := make(chan StreamChunk)

	go func() {
		defer close(ch)
		defer httpResp.Body.Close()

		// These variables accumulate metadata across multiple events.
		// Unlike Gemini (where every event is self-contained), Anthropic
		// spreads the metadata across the stream:
		//   - message_start gives us ID, model, input tokens
		//   - message_delta (near the end) gives us output tokens
		//   - message_stop is the final signal
		//
		// So we need to remember values from earlier events to build
		// the final Done chunk. This is like collecting parts of a
		// response across multiple 'data' events in a Node.js SSE
		// EventSource listener, then assembling them at the end.
		var (
			respID       string
			model        string
			inputTokens  int
			outputTokens int
		)

		scanner := bufio.NewScanner(httpResp.Body)

		for scanner.Scan() {
			line := scanner.Text()

			// Same as Gemini: skip lines that aren't data lines.
			// We ignore the "event: ..." lines entirely because the
			// JSON payload itself contains a "type" field that tells
			// us what kind of event it is. This means we don't need
			// to track state between the event: and data: lines.
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			jsonData := strings.TrimPrefix(line, "data: ")

			// Decode into our wrapper struct. The "type" field tells
			// us which event this is, and only the relevant fields
			// will be populated (the rest stay at their zero values).
			var event anthropicStreamEvent
			if err := json.Unmarshal([]byte(jsonData), &event); err != nil {
				ch <- StreamChunk{
					Done:  true,
					Error: fmt.Errorf("decoding anthropic stream event: %w", err),
				}
				return
			}

			switch event.Type {
			case "message_start":
				// First event in the stream. Grab the response metadata.
				// event.Message contains the ID, model, and input token
				// count (output_tokens is 0 at this point — nothing
				// has been generated yet).
				if event.Message != nil {
					respID = event.Message.ID
					model = event.Message.Model
					inputTokens = event.Message.Usage.InputTokens
				}

			case "content_block_delta":
				// The main event — carries one text token. These arrive
				// rapidly, one per generated token. Each becomes a
				// StreamChunk that flows through the channel to the SSE
				// writer and out to the client.
				if event.Delta == nil {
					continue
				}

				chunk := StreamChunk{
					ID:    respID,
					Model: model,
					Delta: event.Delta.Text,
				}

				select {
				case ch <- chunk:
				case <-ctx.Done():
					return
				}

			case "message_delta":
				// Near-final event. Carries stop_reason and the output
				// token count. We save outputTokens for the final chunk.
				if event.Delta != nil && event.Delta.StopReason != "" {
					// stop_reason arrived — we'll use it on the final chunk
				}
				if event.Usage != nil {
					outputTokens = event.Usage.OutputTokens
				}

			case "message_stop":
				// Final event. Send a Done chunk with the accumulated
				// usage data. This is analogous to the finishReason
				// chunk in Gemini's stream, but the data was collected
				// from earlier events rather than all in one event.
				chunk := StreamChunk{
					ID:    respID,
					Model: model,
					Done:  true,
					Usage: &Usage{
						PromptTokens:     inputTokens,
						CompletionTokens: outputTokens,
						TotalTokens:      inputTokens + outputTokens,
					},
				}

				select {
				case ch <- chunk:
				case <-ctx.Done():
					return
				}

			// Other event types (content_block_start, content_block_stop,
			// ping) don't carry data we need — skip them.
			}
		}

		// Surface scanner errors (same pattern as Google's streaming).
		if err := scanner.Err(); err != nil {
			select {
			case ch <- StreamChunk{
				Done:  true,
				Error: fmt.Errorf("reading anthropic stream: %w", err),
			}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}
