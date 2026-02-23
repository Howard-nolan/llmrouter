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
// GoogleProvider struct + constructor
// ---------------------------------------------------------------------------

// GoogleProvider implements the Provider interface for Google's Gemini API.
// It translates our unified ChatRequest into Gemini's format, makes the
// HTTP call, and translates the response back.
type GoogleProvider struct {
	apiKey  string       // Gemini API key (sent as a query parameter, not a header)
	baseURL string       // e.g. "https://generativelanguage.googleapis.com/v1beta"
	client  *http.Client // reusable HTTP client (manages connection pooling)
}

// NewGoogleProvider creates a GoogleProvider ready to make API calls.
// We take an *http.Client as a parameter instead of creating one internally.
// This is a Go best practice called "dependency injection" — it lets tests
// pass in a fake/mock HTTP client, and lets main.go configure timeouts on
// the client. In Express terms, it's like passing a custom Axios instance
// to a service instead of using the global one.
func NewGoogleProvider(apiKey, baseURL string, client *http.Client) *GoogleProvider {
	return &GoogleProvider{
		apiKey:  apiKey,
		baseURL: baseURL,
		client:  client,
	}
}

// Name returns the provider identifier. Used for logging, metrics, and
// the X-LLMRouter-Provider response header.
func (g *GoogleProvider) Name() string {
	return "google"
}

// ---------------------------------------------------------------------------
// Gemini API types (unexported — only this file uses them)
// ---------------------------------------------------------------------------

// These structs represent the JSON shapes that Gemini's API expects and
// returns. They're private to this adapter. The json struct tags tell Go's
// encoder/decoder how to map struct fields to JSON keys.

// --- Request types ---

// geminiRequest is the top-level request body for Gemini's generateContent.
type geminiRequest struct {
	Contents         []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// geminiContent represents one message in the conversation.
// Gemini uses "parts" (an array) because it supports multimodal input
// (text + images). For text-only, we always send a single part.
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is one piece of content within a message.
// For text, it's just {"text": "..."}.
type geminiPart struct {
	Text string `json:"text"`
}

// geminiGenerationConfig holds generation parameters.
type geminiGenerationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

// --- Response types ---

// geminiResponse is the top-level response from generateContent.
type geminiResponse struct {
	Candidates    []geminiCandidate    `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
}

// geminiCandidate is one generated response. Gemini can return multiple
// candidates, but we only use the first one (like OpenAI's choices[0]).
type geminiCandidate struct {
	Content      geminiContent `json:"content"`
	FinishReason string        `json:"finishReason"`
}

// geminiUsageMetadata holds token counts from the Gemini response.
type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

// toGeminiRequest translates our unified ChatRequest into Gemini's format.
// This is where the three key differences get handled:
//  1. System messages get pulled out into systemInstruction
//  2. Messages become contents with parts
//  3. max_tokens becomes maxOutputTokens inside generationConfig
func toGeminiRequest(req *ChatRequest) *geminiRequest {
	gr := &geminiRequest{}

	// Walk through our messages and sort them into the right place.
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			// Gemini wants system messages in a separate field, not in
			// the contents array. If there are multiple system messages,
			// we concatenate them (Gemini only accepts one systemInstruction).
			if gr.SystemInstruction == nil {
				gr.SystemInstruction = &geminiContent{
					Parts: []geminiPart{{Text: msg.Content}},
				}
			} else {
				// Append to existing system instruction.
				gr.SystemInstruction.Parts = append(
					gr.SystemInstruction.Parts,
					geminiPart{Text: msg.Content},
				)
			}
			continue
		}

		// Map roles: OpenAI uses "assistant", Gemini uses "model".
		role := msg.Role
		if role == "assistant" {
			role = "model"
		}

		gr.Contents = append(gr.Contents, geminiContent{
			Role:  role,
			Parts: []geminiPart{{Text: msg.Content}},
		})
	}

	// Set generation config if max_tokens was specified.
	// In Go, the zero value for int is 0, so we check > 0 to know
	// if the caller actually set it (like checking !== undefined in JS).
	if req.MaxTokens > 0 {
		gr.GenerationConfig = &geminiGenerationConfig{
			MaxOutputTokens: req.MaxTokens,
		}
	}

	return gr
}

// ---------------------------------------------------------------------------
// Non-streaming: ChatCompletion
// ---------------------------------------------------------------------------

// ChatCompletion sends a non-streaming request to Gemini's generateContent
// endpoint and returns the complete response.
//
// The flow: translate request → HTTP POST → read response → translate back.
func (g *GoogleProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	// Step 1: Translate our unified request into Gemini's format.
	geminiReq := toGeminiRequest(req)

	// Step 2: Serialize the Gemini request to JSON bytes.
	// json.Marshal is like JSON.stringify() in JS — it converts a Go
	// value into a []byte of JSON.
	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Step 3: Build the HTTP request.
	// The Gemini endpoint pattern is: {baseURL}/models/{model}:generateContent
	// The API key goes as a query parameter (?key=...), which is unusual —
	// most APIs put it in an Authorization header.
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s",
		g.baseURL, req.Model, g.apiKey,
	)

	// http.NewRequestWithContext creates an HTTP request that's tied to our
	// context. If the context gets cancelled (client disconnects, timeout),
	// the HTTP call will be aborted automatically.
	//
	// bytes.NewReader(body) wraps our JSON bytes in a reader — Go's HTTP
	// client needs an io.Reader for the request body, not raw bytes.
	// This is like how fetch() needs a ReadableStream or string for body.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Step 4: Make the HTTP call.
	// g.client.Do(httpReq) sends the request and returns the response.
	// This blocks until the full response arrives (since we're non-streaming).
	httpResp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to gemini: %w", err)
	}
	// defer runs when the enclosing function returns — no matter how it
	// returns (normal return, error return, even panic). It's Go's way of
	// saying "clean this up when we're done." Like a finally block in JS.
	//
	// We MUST close the response body or we'll leak TCP connections.
	// The HTTP client can't reuse the connection until the body is fully
	// read and closed.
	defer httpResp.Body.Close()

	// Step 5: Check for HTTP errors.
	if httpResp.StatusCode != http.StatusOK {
		// Read the error body for debugging info.
		var errBody map[string]any
		json.NewDecoder(httpResp.Body).Decode(&errBody)
		return nil, fmt.Errorf("gemini API error (status %d): %v",
			httpResp.StatusCode, errBody,
		)
	}

	// Step 6: Decode the JSON response into our Gemini response struct.
	// json.NewDecoder reads from an io.Reader (the response body) and
	// decodes JSON into the target struct. It's like:
	//   const data = await response.json()
	// except it reads from a stream into a typed struct.
	var geminiResp geminiResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&geminiResp); err != nil {
		return nil, fmt.Errorf("decoding gemini response: %w", err)
	}

	// Step 7: Translate the Gemini response back into our unified format.
	// Check that we got at least one candidate with content.
	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("gemini returned no candidates")
	}

	candidate := geminiResp.Candidates[0]

	// Build the unified response. We extract the text from the first
	// part of the first candidate (Gemini can return multi-part responses
	// for multimodal, but for text it's always a single part).
	resp := &ChatResponse{
		Model:   req.Model,
		Content: candidate.Content.Parts[0].Text,
	}

	// Map usage metadata if present.
	if geminiResp.UsageMetadata != nil {
		resp.Usage = Usage{
			PromptTokens:     geminiResp.UsageMetadata.PromptTokenCount,
			CompletionTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      geminiResp.UsageMetadata.TotalTokenCount,
		}
	}

	return resp, nil
}

// ---------------------------------------------------------------------------
// Streaming: ChatCompletionStream
// ---------------------------------------------------------------------------

// ChatCompletionStream sends a streaming request to Gemini's
// streamGenerateContent endpoint and returns a channel of StreamChunks.
//
// The flow:
//  1. Translate request (same as non-streaming)
//  2. POST to streamGenerateContent?alt=sse (instead of generateContent)
//  3. Spin up a goroutine that reads SSE lines from the response body
//  4. Return the channel immediately — the caller reads chunks from it
//
// The goroutine + channel pattern here is like returning a ReadableStream
// in Node.js. The caller doesn't wait for the full response — they get
// chunks as they arrive.
func (g *GoogleProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	// Step 1: Translate request (reuse the same translation as non-streaming).
	geminiReq := toGeminiRequest(req)

	body, err := json.Marshal(geminiReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	// Step 2: Build the HTTP request to the STREAMING endpoint.
	// Note the different path: streamGenerateContent instead of generateContent.
	// The ?alt=sse query parameter tells Gemini to return Server-Sent Events
	// instead of a single JSON blob.
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse&key=%s",
		g.baseURL, req.Model, g.apiKey,
	)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Step 3: Make the HTTP call.
	// Unlike the non-streaming path, we do NOT defer Body.Close() here.
	// The response body stays open — it's a long-lived stream. The
	// goroutine we launch below will close it when it's done reading.
	httpResp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request to gemini: %w", err)
	}

	// Check for HTTP errors BEFORE we start the goroutine.
	// If the API returned an error (like 401 or 429), we want to report
	// it immediately, not inside the goroutine where it's harder to
	// surface to the caller.
	if httpResp.StatusCode != http.StatusOK {
		defer httpResp.Body.Close()
		var errBody map[string]any
		json.NewDecoder(httpResp.Body).Decode(&errBody)
		return nil, fmt.Errorf("gemini API error (status %d): %v",
			httpResp.StatusCode, errBody,
		)
	}

	// Step 4: Create the channel and launch the goroutine.
	//
	// make(chan StreamChunk) creates an UNBUFFERED channel — sending
	// blocks until someone receives. This provides natural backpressure:
	// the goroutine won't read the next SSE event until the handler has
	// consumed the current chunk (like a pipe in Node streams with
	// highWaterMark=1).
	//
	// We could use a buffered channel like make(chan StreamChunk, 10) to
	// let the goroutine read ahead, but unbuffered is simpler and keeps
	// memory predictable.
	ch := make(chan StreamChunk)

	// go func() { ... }() launches an anonymous function as a goroutine.
	// This is Go's concurrency primitive — it's like calling an async
	// function that runs concurrently but without the await syntax.
	//
	// Think of it as: (async () => { /* runs in background */ })()
	// except it's truly concurrent (not just async I/O), and we
	// communicate results via the channel instead of resolving a promise.
	go func() {
		// defer runs when this goroutine exits (for any reason).
		// We MUST close both the channel and the response body.
		//
		// Closing the channel signals to the consumer (the handler)
		// that no more chunks are coming. A for-range loop over a
		// channel exits automatically when the channel is closed.
		// If we forgot to close it, the handler would block forever
		// waiting for more chunks — a goroutine leak.
		defer close(ch)
		defer httpResp.Body.Close()

		// bufio.NewScanner wraps the response body in a line scanner.
		// scanner.Scan() reads one line at a time (up to \n), returning
		// true if a line was read, false on EOF or error.
		//
		// This is like readline.createInterface({ input: stream })
		// in Node.js, where you'd do: rl.on('line', (line) => {...})
		scanner := bufio.NewScanner(httpResp.Body)

		for scanner.Scan() {
			line := scanner.Text()

			// SSE format sends lines like:
			//   data: {"candidates":[...]}\n
			//   \n
			//   data: {"candidates":[...]}\n
			//   \n
			//
			// Blank lines separate events. Lines without the "data: "
			// prefix are either blank separators or SSE comments (lines
			// starting with ":") — we skip them.
			if !strings.HasPrefix(line, "data: ") {
				continue
			}

			// Strip the "data: " prefix to get the raw JSON.
			// strings.TrimPrefix returns the string unchanged if the
			// prefix isn't present (but we already checked with HasPrefix).
			jsonData := strings.TrimPrefix(line, "data: ")

			// Decode the JSON into the same geminiResponse struct we
			// use for non-streaming. Gemini sends the same structure
			// for each SSE event — the only difference is each event
			// contains just one token/chunk of text instead of the
			// full response.
			var geminiResp geminiResponse
			if err := json.Unmarshal([]byte(jsonData), &geminiResp); err != nil {
				// If we can't parse a line, send an error chunk and stop.
				// The Done: true tells the consumer this stream is over.
				ch <- StreamChunk{
					Done:  true,
					Error: fmt.Errorf("decoding gemini stream event: %w", err),
				}
				return
			}

			// Extract the text delta from the response.
			// Same structure as non-streaming: candidates[0].content.parts[0].text
			if len(geminiResp.Candidates) == 0 {
				continue
			}
			candidate := geminiResp.Candidates[0]

			var delta string
			if len(candidate.Content.Parts) > 0 {
				delta = candidate.Content.Parts[0].Text
			}

			// Build the StreamChunk.
			chunk := StreamChunk{
				Model: req.Model,
				Delta: delta,
			}

			// Check if this is the final chunk. Gemini sets finishReason
			// to "STOP" (or other values like "MAX_TOKENS") on the last
			// candidate. An empty finishReason means more chunks are coming.
			if candidate.FinishReason != "" {
				chunk.Done = true

				// Usage metadata is typically included in the final event.
				if geminiResp.UsageMetadata != nil {
					chunk.Usage = &Usage{
						PromptTokens:     geminiResp.UsageMetadata.PromptTokenCount,
						CompletionTokens: geminiResp.UsageMetadata.CandidatesTokenCount,
						TotalTokens:      geminiResp.UsageMetadata.TotalTokenCount,
					}
				}
			}

			// Send the chunk on the channel. This blocks until the
			// consumer reads it (unbuffered channel = synchronous handoff).
			//
			// The select statement lets us also check if the context
			// was cancelled. select is like Promise.race() — it waits
			// for whichever case happens first.
			//
			// Without this select, if the client disconnects, we'd keep
			// reading from Gemini and trying to send on the channel with
			// nobody listening — wasting resources.
			select {
			case ch <- chunk:
				// Chunk sent successfully, continue to next line.
			case <-ctx.Done():
				// Context cancelled (client disconnected or timeout).
				// ctx.Done() returns a channel that gets closed when
				// the context is cancelled. When it closes, this case
				// fires. We just return, which triggers the deferred
				// close(ch) and httpResp.Body.Close().
				return
			}
		}

		// If the scanner stopped due to an I/O error (not just EOF),
		// surface it. scanner.Err() returns nil on clean EOF.
		if err := scanner.Err(); err != nil {
			// Only send if context isn't cancelled (avoid sending on
			// a channel nobody is reading).
			select {
			case ch <- StreamChunk{
				Done:  true,
				Error: fmt.Errorf("reading gemini stream: %w", err),
			}:
			case <-ctx.Done():
			}
		}
	}()

	// Return the channel immediately. The goroutine is now running in
	// the background, reading from Gemini and sending chunks. The caller
	// will do: for chunk := range ch { /* process each chunk */ }
	return ch, nil
}
