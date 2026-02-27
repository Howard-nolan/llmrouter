package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/howard-nolan/llmrouter/internal/provider"
	"github.com/howard-nolan/llmrouter/internal/stream"
)

// resolveProvider looks up the Provider for a given model name using the
// model-to-provider registry. Returns an error if the model isn't known.
//
// This is the core of the provider dispatch: the client sends us a model
// name like "gemini-2.0-flash" or "claude-haiku-4-5-20251001", and we
// need to find which Provider handles it. The s.models map was built at
// startup from the config file's provider → models lists, so this is
// just a map lookup.
//
// In Express terms, this is like a middleware that inspects req.body.model
// and attaches the right service client to the request context.
func (s *Server) resolveProvider(model string) (provider.Provider, error) {
	p, ok := s.models[model]
	if !ok {
		return nil, fmt.Errorf("unknown model: %q", model)
	}
	return p, nil
}

// handleHealth responds with a simple JSON status indicating the server
// is alive. Later we'll expand this to check provider connectivity, Redis,
// etc. — but for now it's a basic liveness probe.
//
// In Express terms, this is like:
//   app.get('/health', (req, res) => res.json({ status: 'ok' }))
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Set the Content-Type header BEFORE calling WriteHeader or Write.
	// In Go, headers must be set before the first write — once you start
	// writing the body, headers are locked in (sent over the wire).
	w.Header().Set("Content-Type", "application/json")

	// json.NewEncoder(w) creates a JSON encoder that writes directly to
	// the ResponseWriter. Encode() serializes the value and writes it.
	// This is the Go equivalent of res.json({...}) in Express, but split
	// into two explicit steps: set the header, then encode the body.
	//
	// We're passing an anonymous struct here — a quick throwaway type
	// defined inline. It's like writing { status: "ok" } as an object
	// literal in JS, except Go needs the field types declared.
	// The `json:"status"` part is a "struct tag" — it tells the JSON
	// encoder to use "status" as the key name (lowercase) instead of
	// the Go field name "Status" (uppercase).
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
	})
}

// handleChatCompletions handles POST /v1/chat/completions.
// It decodes the request, resolves the provider from the model name,
// and dispatches to either the streaming or non-streaming path.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Step 1: Decode the incoming JSON body into our unified ChatRequest.
	var req provider.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
		return
	}

	// Step 2: Resolve the provider from the model name.
	// This is the registry lookup — "gemini-2.0-flash" → GoogleProvider,
	// "claude-haiku-4-5-20251001" → AnthropicProvider, etc.
	p, err := s.resolveProvider(req.Model)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	// Step 3: Set response headers so the client knows which provider
	// and model handled the request. These are useful for debugging
	// and will be essential once we add "model": "auto" routing —
	// the client won't know which model was selected without these.
	w.Header().Set("X-LLMRouter-Provider", p.Name())
	w.Header().Set("X-LLMRouter-Model", req.Model)

	// Step 4: Branch on streaming vs non-streaming.
	if req.Stream {
		chunks, err := p.ChatCompletionStream(r.Context(), &req)
		if err != nil {
			log.Printf("provider stream error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "provider error: " + err.Error(),
			})
			return
		}

		if err := stream.Write(w, chunks); err != nil {
			log.Printf("stream write error: %v", err)
		}
		return
	}

	// Non-streaming path.
	resp, err := p.ChatCompletion(r.Context(), &req)
	if err != nil {
		log.Printf("provider error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "provider error: " + err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
