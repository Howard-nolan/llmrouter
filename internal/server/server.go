// Package server sets up the HTTP router, middleware, and request handlers.
package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/howard-nolan/llmrouter/internal/config"
	"github.com/howard-nolan/llmrouter/internal/provider"
)

// Server holds the HTTP router and all dependencies that handlers need.
// As we add more features (provider registry, cache, embedder), they'll
// become fields here — similar to attaching services to an Express app.
type Server struct {
	router   chi.Router
	cfg      *config.Config
	provider provider.Provider
}

// New creates a Server, wires up routes and middleware, and returns it
// ready to use as an http.Handler. This is Go's equivalent of a
// constructor — the convention is to name it New when the package name
// already tells you what you're constructing (server.New → "new server").
func New(cfg *config.Config, p provider.Provider) *Server {
	s := &Server{cfg: cfg, provider: p}
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
