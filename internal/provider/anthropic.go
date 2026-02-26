package provider

import (
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
