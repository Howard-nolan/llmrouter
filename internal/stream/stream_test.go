package stream

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/howard-nolan/llmrouter/internal/provider"
)

// sendChunks is a test helper that sends chunks on a channel in a goroutine
// and closes the channel when done. This simulates what the provider adapter
// does in production.
func sendChunks(chunks ...provider.StreamChunk) <-chan provider.StreamChunk {
	ch := make(chan provider.StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range chunks {
			ch <- c
		}
	}()
	return ch
}

// parseSSEEvents splits the raw SSE output into individual data payloads,
// excluding the "data: [DONE]" sentinel.
func parseSSEEvents(body string) []string {
	var events []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload != "[DONE]" {
				events = append(events, payload)
			}
		}
	}
	return events
}

func TestWrite_MultipleChunks(t *testing.T) {
	ch := sendChunks(
		provider.StreamChunk{Model: "test-model", Delta: "Hello"},
		provider.StreamChunk{Model: "test-model", Delta: " world"},
		provider.StreamChunk{Model: "test-model", Done: true, Usage: &provider.Usage{
			PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7,
		}},
	)

	w := httptest.NewRecorder()
	err := Write(w, ch)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	// Verify SSE headers.
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}

	body := w.Body.String()

	// Should end with [DONE].
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("missing [DONE] sentinel")
	}

	// Parse the JSON events.
	events := parseSSEEvents(body)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}

	// First event: content "Hello".
	var first sseChunk
	if err := json.Unmarshal([]byte(events[0]), &first); err != nil {
		t.Fatalf("failed to parse event 0: %v", err)
	}
	if first.Choices[0].Delta.Content != "Hello" {
		t.Errorf("event 0 content = %q, want %q", first.Choices[0].Delta.Content, "Hello")
	}
	if first.Choices[0].FinishReason != nil {
		t.Errorf("event 0 finish_reason = %v, want nil", *first.Choices[0].FinishReason)
	}

	// Second event: content " world".
	var second sseChunk
	if err := json.Unmarshal([]byte(events[1]), &second); err != nil {
		t.Fatalf("failed to parse event 1: %v", err)
	}
	if second.Choices[0].Delta.Content != " world" {
		t.Errorf("event 1 content = %q, want %q", second.Choices[0].Delta.Content, " world")
	}

	// Third event: finish with usage.
	var third sseChunk
	if err := json.Unmarshal([]byte(events[2]), &third); err != nil {
		t.Fatalf("failed to parse event 2: %v", err)
	}
	if third.Choices[0].FinishReason == nil || *third.Choices[0].FinishReason != "stop" {
		t.Error("event 2 should have finish_reason=stop")
	}
	if third.Choices[0].Delta.Content != "" {
		t.Errorf("event 2 delta should be empty, got %q", third.Choices[0].Delta.Content)
	}
	if third.Usage == nil {
		t.Fatal("event 2 should have usage")
	}
	if third.Usage.TotalTokens != 7 {
		t.Errorf("usage total_tokens = %d, want 7", third.Usage.TotalTokens)
	}
}

func TestWrite_FinalChunkWithContent(t *testing.T) {
	// Simulates Gemini sending content + finishReason in the same event.
	ch := sendChunks(
		provider.StreamChunk{
			Model: "test-model",
			Delta: "Paris is the capital.",
			Done:  true,
			Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	)

	w := httptest.NewRecorder()
	err := Write(w, ch)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	events := parseSSEEvents(w.Body.String())

	// Should produce two events: content, then finish.
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	// First event should have the content.
	var content sseChunk
	if err := json.Unmarshal([]byte(events[0]), &content); err != nil {
		t.Fatalf("failed to parse content event: %v", err)
	}
	if content.Choices[0].Delta.Content != "Paris is the capital." {
		t.Errorf("content = %q, want %q", content.Choices[0].Delta.Content, "Paris is the capital.")
	}
	if content.Choices[0].FinishReason != nil {
		t.Error("content event should not have finish_reason")
	}

	// Second event should have finish_reason and empty delta.
	var finish sseChunk
	if err := json.Unmarshal([]byte(events[1]), &finish); err != nil {
		t.Fatalf("failed to parse finish event: %v", err)
	}
	if finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "stop" {
		t.Error("finish event should have finish_reason=stop")
	}
	if finish.Choices[0].Delta.Content != "" {
		t.Errorf("finish event delta should be empty, got %q", finish.Choices[0].Delta.Content)
	}
	if finish.Usage == nil || finish.Usage.TotalTokens != 15 {
		t.Errorf("finish event should have usage with total_tokens=15")
	}
}

func TestWrite_MidStreamError(t *testing.T) {
	ch := sendChunks(
		provider.StreamChunk{Model: "test-model", Delta: "partial"},
		provider.StreamChunk{Done: true, Error: fmt.Errorf("connection reset")},
	)

	w := httptest.NewRecorder()
	err := Write(w, ch)

	// Should return the error.
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "connection reset")
	}

	// Should NOT contain [DONE] since the stream errored.
	if strings.Contains(w.Body.String(), "[DONE]") {
		t.Error("errored stream should not contain [DONE]")
	}
}

func TestWrite_SSEFormat(t *testing.T) {
	// Verify the raw SSE format: every event should be "data: ...\n\n".
	ch := sendChunks(
		provider.StreamChunk{Model: "m", Delta: "hi"},
		provider.StreamChunk{Model: "m", Done: true},
	)

	w := httptest.NewRecorder()
	if err := Write(w, ch); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}

	body := w.Body.String()

	// Every "data:" line should be followed by a blank line (double \n).
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Error("missing properly formatted [DONE] sentinel")
	}

	// Each event should be separated by double newlines.
	parts := strings.Split(body, "\n\n")
	// Last element is empty (trailing \n\n), so we expect at least 3 non-empty parts:
	// event 1, finish event, [DONE].
	nonEmpty := 0
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 3 {
		t.Errorf("got %d SSE events, want 3 (content + finish + DONE)", nonEmpty)
	}
}
