package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/howard-nolan/llmrouter/internal/provider"
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

	// Step 2: For now, only handle non-streaming requests.
	if req.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "streaming not implemented yet",
		})
		return
	}

	// Step 3: Call the provider's non-streaming method.
	// r.Context() gets the request's context — it automatically cancels
	// if the client disconnects. We pass it through so the provider's
	// HTTP call to Gemini will also be cancelled.
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

	// Step 4: Return the response as JSON.
	// For now we return our internal format directly. Later we'll
	// translate this into the full OpenAI response shape.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
