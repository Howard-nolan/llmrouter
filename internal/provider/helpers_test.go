package provider

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/cassette"
	"gopkg.in/dnaeon/go-vcr.v4/pkg/recorder"
)

// newReplayClient creates an *http.Client backed by go-vcr in replay-only
// mode. It loads a cassette (recorded HTTP fixture) from the testdata
// directory and replays the stored response instead of making a real network
// call.
//
// In Node.js terms, this is like nock(...).reply(...) but loaded from a
// YAML file. The key insight: go-vcr implements http.RoundTripper (the
// interface that http.Client uses internally to execute requests), so we
// can swap it into the client's Transport field. Our provider constructors
// already accept *http.Client — this is why dependency injection matters.
//
// Parameters:
//   - t: the test instance (used for cleanup and fatal errors)
//   - cassette: name of the cassette file (without directory prefix),
//     e.g. "google_chat_completion" loads testdata/cassettes/google_chat_completion.yaml
func newReplayClient(t *testing.T, cassetteName string) *http.Client {
	// t.Helper() marks this function as a test helper. When a test fails
	// inside this function, Go reports the failure at the CALLER's line
	// number, not inside this helper. Without it, every failure would
	// point to the require.NoError line below, which isn't useful.
	// In Jest terms, it's like Error.captureStackTrace skipping frames.
	t.Helper()

	// Build the path to the cassette file. filepath.Join handles
	// OS-specific path separators (/ on Mac/Linux, \ on Windows).
	// Go tests run with the working directory set to the package
	// directory, so "testdata/cassettes/..." resolves correctly.
	cassettePath := filepath.Join("testdata", "cassettes", cassetteName)

	// Create the recorder with three options:
	//
	// ModeReplayOnly: NEVER make real HTTP calls. If the cassette
	// doesn't have a matching interaction, the test fails immediately
	// with a clear error. This is critical for CI — we never want
	// tests accidentally hitting real APIs.
	//
	// WithSkipRequestLatency: Cassettes store the original response
	// duration. Without this option, go-vcr would sleep for that
	// duration during replay (to simulate real latency). We skip it
	// because we want fast tests.
	//
	// WithMatcher: go-vcr's default matcher checks EVERY field on the
	// request (Method, URL, Proto, Headers, Body, Host, ContentLength,
	// etc.). That works great for cassettes recorded from real API
	// calls (where all fields are populated automatically), but our
	// cassettes are hand-crafted — we only fill in the fields we care
	// about. So we use a custom matcher that checks only Method + URL.
	//
	// cassette.Request is the cassette's representation of a request
	// (the YAML you see in the fixture file). *http.Request is the
	// actual request the adapter builds at runtime. The matcher returns
	// true when they should be considered "the same interaction."
	r, err := recorder.New(cassettePath,
		recorder.WithMode(recorder.ModeReplayOnly),
		recorder.WithSkipRequestLatency(true),
		recorder.WithMatcher(func(r *http.Request, cr cassette.Request) bool {
			return r.Method == cr.Method && r.URL.String() == cr.URL
		}),
	)
	require.NoError(t, err)

	// t.Cleanup registers a function to run when the test finishes
	// (pass or fail). It's like afterEach() in Jest. We use it to
	// call r.Stop(), which closes the recorder's resources.
	//
	// Why t.Cleanup instead of defer? In Go, defer runs when the
	// enclosing FUNCTION returns. But test helpers are called from
	// test functions — defer here would run when newReplayClient
	// returns (immediately), not when the test finishes. t.Cleanup
	// is tied to the test's lifecycle, not the function's.
	t.Cleanup(func() {
		require.NoError(t, r.Stop())
	})

	// Wrap the recorder as the client's Transport. The recorder
	// intercepts every HTTP request, matches it against the cassette,
	// and returns the stored response. The actual http.DefaultTransport
	// (which does real TCP connections) is never used.
	return &http.Client{Transport: r}
}
