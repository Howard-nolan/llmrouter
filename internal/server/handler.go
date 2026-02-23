package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/howard-nolan/llmrouter/internal/provider"
	"github.com/howard-nolan/llmrouter/internal/stream"
)

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
// It decodes the request, calls the provider, and returns the response.
// For now this only handles the non-streaming path — streaming comes next.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	// Step 1: Decode the incoming JSON body into our unified ChatRequest.
	// json.NewDecoder(r.Body) reads from the request body stream.
	// This is like: const body = await req.json() in Node/Express.
	var req provider.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
		return
	}

	// Step 2: Branch on streaming vs non-streaming.
	if req.Stream {
		// Get the chunk channel from the provider.
		chunks, err := s.provider.ChatCompletionStream(r.Context(), &req)
		if err != nil {
			log.Printf("provider stream error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "provider error: " + err.Error(),
			})
			return
		}

		// Write SSE events to the client. This blocks until the
		// channel closes (stream complete) or an error occurs.
		if err := stream.Write(w, chunks); err != nil {
			log.Printf("stream write error: %v", err)
		}
		return
	}

	// Non-streaming path.
	resp, err := s.provider.ChatCompletion(r.Context(), &req)
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
