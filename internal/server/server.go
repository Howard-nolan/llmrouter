// Package server sets up the HTTP router, middleware, and request handlers.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/howard-nolan/llmrouter/internal/cache"
	"github.com/howard-nolan/llmrouter/internal/config"
	"github.com/howard-nolan/llmrouter/internal/provider"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Embedder is the interface for computing text embeddings. Defined here
// (at the consumer) rather than in the embedder package to avoid pulling
// in CGo native dependencies when importing the server package. The real
// implementation (*embedder.ONNXEmbedder) satisfies this implicitly.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// ModelRouter selects a concrete model name for "auto" routing requests.
// Defined here at the consumer to decouple the server package from the
// router package (same pattern as Embedder above).
type ModelRouter interface {
	Route(embedding []float32, strategy string, providerName string) (string, error)
	CheapAndQualityFor(providerName string) (cheap, quality string, ok bool)
}

// Server holds the HTTP router and all dependencies that handlers need.
type Server struct {
	router      chi.Router
	cfg         *config.Config
	models      map[string]provider.Provider
	embedder    Embedder
	cache       cache.Cache
	modelRouter ModelRouter
}

// New creates a Server with all dependencies wired in.
func New(cfg *config.Config, models map[string]provider.Provider, emb Embedder, c cache.Cache, mr ModelRouter) *Server {
	s := &Server{
		cfg:         cfg,
		models:      models,
		embedder:    emb,
		cache:       c,
		modelRouter: mr,
	}
	s.routes()
	return s
}

// routes builds the chi router with all middleware and route definitions.
// This is conceptually like your Express app.use() / app.get() / app.post()
// setup, but gathered in one method so the routing table is easy to scan.
func (s *Server) routes() {
	r := chi.NewRouter()

	// --- Global middleware ---
	// middleware.Logger prints a log line for every request, similar to
	// morgan('dev') in Express. It logs method, path, status, and duration.
	r.Use(middleware.Logger)

	// middleware.Recoverer catches panics in handlers and returns a 500
	// instead of crashing the whole process. In Express, you'd use an
	// error-handling middleware like app.use((err, req, res, next) => ...).
	r.Use(middleware.Recoverer)

	// --- Routes ---
	r.Get("/health", s.handleHealth)
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/cache/stats", s.handleCacheStats)
	r.Post("/cache/flush", s.handleCacheFlush)
	r.Post("/v1/chat/completions", s.handleChatCompletions)

	s.router = r
}

// ServeHTTP makes Server satisfy the http.Handler interface. Every incoming
// request flows through this method, and we just delegate to chi's router.
//
// This is what allows main.go to pass our Server directly to
// http.Server{Handler: srv} — the stdlib needs anything that has a
// ServeHTTP(ResponseWriter, *Request) method.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}
