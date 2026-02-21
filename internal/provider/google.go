package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
// Streaming: ChatCompletionStream (to be implemented next)
// ---------------------------------------------------------------------------

// ChatCompletionStream sends a streaming request to Gemini's
// streamGenerateContent endpoint and returns a channel of StreamChunks.
func (g *GoogleProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error) {
	// TODO: implement streaming
	return nil, fmt.Errorf("streaming not implemented yet")
}
