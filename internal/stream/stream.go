// Package stream handles SSE writing, response buffering, and token-level metrics.
package stream

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/howard-nolan/llmrouter/internal/provider"
)

// ---------------------------------------------------------------------------
// OpenAI-compatible SSE response types
// ---------------------------------------------------------------------------

// These structs define the JSON shape that OpenAI-compatible clients expect
// to receive in each SSE event during streaming. Our API surface matches
// the OpenAI format, so we translate our internal StreamChunk into this
// shape before sending it to the client.
//
// The OpenAI streaming format looks like:
//   data: {"id":"...","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hi"}}]}
//
// We need these structs because json.Marshal needs a Go type to serialize.
// They're private to this package — no other code needs to know about
// the wire format details.

// sseChunk is the top-level JSON object in each SSE event.
type sseChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`

	// Usage is included only on the final chunk (when it's available).
	// The pointer + omitempty combo means: if Usage is nil, don't include
	// the "usage" key in the JSON at all. This matches OpenAI's behavior
	// where usage only appears on the last event.
	Usage *sseUsage `json:"usage,omitempty"`
}

// sseChoice represents one choice in the streaming response.
// OpenAI supports multiple choices (n > 1), but we always return one.
type sseChoice struct {
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`

	// FinishReason is null for all chunks except the final one.
	// We use *string (pointer to string) so we can distinguish between
	// "not set" (nil → renders as JSON null) and "set to a value"
	// (like "stop"). A plain string can't represent null in JSON —
	// it would serialize as "" (empty string), which is wrong.
	FinishReason *string `json:"finish_reason"`
}

// sseDelta holds the incremental content in each chunk.
// On non-final chunks, Content has the text fragment.
// On the final chunk, Content is typically empty.
type sseDelta struct {
	// Content is omitempty so that the final chunk sends {"delta":{}}
	// instead of {"delta":{"content":""}} — matching OpenAI's format.
	Content string `json:"content,omitempty"`
}

// sseUsage mirrors provider.Usage for the JSON response.
type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ---------------------------------------------------------------------------
// SSE Writer
// ---------------------------------------------------------------------------

// Write reads StreamChunks from the channel and writes them to the
// http.ResponseWriter as OpenAI-compatible Server-Sent Events.
//
// This is the consumer side of the streaming pipeline:
//   Google goroutine → channel → Write() → http.ResponseWriter → client
//
// It sets the SSE headers, then loops over the channel, formatting each
// chunk as a "data: {json}\n\n" line and flushing it immediately so the
// client sees tokens arrive in real-time.
func Write(w http.ResponseWriter, chunks <-chan provider.StreamChunk) error {
	// --- Step 1: Assert that the ResponseWriter supports flushing ---
	//
	// http.ResponseWriter is an interface with three methods: Header(),
	// Write(), and WriteHeader(). But the concrete type that Go's HTTP
	// server passes to handlers ALSO implements http.Flusher (which adds
	// a Flush() method). We need Flush() to push each SSE event to the
	// client immediately instead of waiting for the buffer to fill.
	//
	// w.(http.Flusher) is a "type assertion" — it checks at runtime
	// whether the value behind the interface also implements another
	// interface. It's like TypeScript's:
	//   if ('flush' in res) { res.flush() }
	//
	// The two-value form (flusher, ok) is a safe assertion — if it
	// fails, ok is false and we don't panic. The single-value form
	// (flusher := w.(http.Flusher)) would panic on failure.
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support flushing (http.Flusher)")
	}

	// --- Step 2: Set SSE headers ---
	//
	// These headers tell the client (and any proxies in between) that
	// this response is a Server-Sent Event stream:
	//
	// Content-Type: text/event-stream — identifies the SSE protocol.
	//   The client (curl -N, EventSource, etc.) uses this to know it
	//   should read the response as a stream of events, not wait for
	//   the full body.
	//
	// Cache-Control: no-cache — tells proxies/browsers not to cache
	//   this response. Caching a stream would break real-time delivery.
	//
	// Connection: keep-alive — keeps the TCP connection open. Without
	//   this, some proxies might close the connection after the first
	//   chunk, thinking the response is complete.
	//
	// These headers MUST be set before any call to Write() or Flush().
	// Once you start writing the body, headers are locked in (sent over
	// the wire). This is the same as in Express — res.setHeader() must
	// come before res.write().
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// --- Step 3: Read chunks from the channel and write SSE events ---
	//
	// for chunk := range chunks reads from the channel until it's closed.
	// Each iteration blocks until the next chunk is available (sent by
	// the Google goroutine). When the goroutine closes the channel
	// (via defer close(ch)), this loop exits.
	//
	// This is the consumer end of the kitchen/waiter pattern from
	// google.go — we're the waiter picking dishes off the serving window.
	for chunk := range chunks {
		// Check for mid-stream errors from the provider goroutine.
		if chunk.Error != nil {
			log.Printf("stream error: %v", chunk.Error)
			// We've already started writing the response (headers sent),
			// so we can't change the status code to 500. The best we can
			// do in SSE is stop sending events. The client will see the
			// stream end unexpectedly — they can detect this because they
			// won't get the "data: [DONE]" sentinel.
			return chunk.Error
		}

		// Build the OpenAI-compatible SSE chunk JSON.
		event := sseChunk{
			ID:     chunk.ID,
			Object: "chat.completion.chunk",
			Model:  chunk.Model,
			Choices: []sseChoice{
				{
					Index: 0,
					Delta: sseDelta{Content: chunk.Delta},
				},
			},
		}

		// On the final chunk, set finish_reason and include usage.
		// If the final chunk also has content (Gemini sometimes sends
		// text and finishReason in the same event), emit the content
		// event first, then a separate finish event.
		if chunk.Done {
			if chunk.Delta != "" {
				// Flush the content event before the finish event.
				jsonBytes, err := json.Marshal(event)
				if err != nil {
					return fmt.Errorf("marshaling SSE chunk: %w", err)
				}
				if _, err := fmt.Fprintf(w, "data: %s\n\n", jsonBytes); err != nil {
					return fmt.Errorf("writing SSE event: %w", err)
				}
				flusher.Flush()
			}

			// Build the finish event with empty delta.
			reason := "stop"
			event.Choices[0].FinishReason = &reason
			event.Choices[0].Delta = sseDelta{}

			if chunk.Usage != nil {
				event.Usage = &sseUsage{
					PromptTokens:     chunk.Usage.PromptTokens,
					CompletionTokens: chunk.Usage.CompletionTokens,
					TotalTokens:      chunk.Usage.TotalTokens,
				}
			}
		}

		// Serialize the event to JSON.
		jsonBytes, err := json.Marshal(event)
		if err != nil {
			log.Printf("failed to marshal SSE chunk: %v", err)
			return fmt.Errorf("marshaling SSE chunk: %w", err)
		}

		// Write the SSE event in the standard format: "data: {json}\n\n"
		//
		// fmt.Fprintf writes formatted text directly to the ResponseWriter.
		// The double newline (\n\n) is required by the SSE spec — it marks
		// the end of an event. A single \n separates fields within an event
		// (like "event:" and "data:" lines), but the blank line (\n\n) is
		// what tells the client "this event is complete, process it."
		//
		// In Node.js, this would be: res.write(`data: ${json}\n\n`)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", jsonBytes); err != nil {
			return fmt.Errorf("writing SSE event: %w", err)
		}

		// Flush immediately. Without this, Go's HTTP server buffers the
		// output and the client wouldn't see tokens until the buffer fills
		// (typically 4KB) or the handler returns. Flushing after every
		// event gives us real-time token delivery.
		//
		// In Node.js, res.write() flushes automatically (no buffering by
		// default). In Go, you have to explicitly ask for it.
		flusher.Flush()
	}

	// --- Step 4: Send the [DONE] sentinel ---
	//
	// After all chunks have been sent (channel closed), we send one final
	// line: "data: [DONE]". This is an OpenAI convention that tells the
	// client the stream is complete. It's not valid JSON — it's a special
	// sentinel string. Clients like the OpenAI Python/JS SDKs look for
	// this to know they should stop reading.
	if _, err := fmt.Fprintf(w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("writing SSE done marker: %w", err)
	}
	flusher.Flush()

	return nil
}
