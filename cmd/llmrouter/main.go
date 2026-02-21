// Package main is the entry point for the llmrouter gateway.
package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/howard-nolan/llmrouter/internal/config"
	"github.com/howard-nolan/llmrouter/internal/provider"
	"github.com/howard-nolan/llmrouter/internal/server"
)

func main() {
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	// Create the Google provider using config values.
	// We pass http.DefaultClient for now — it has sensible defaults
	// (connection pooling, no timeout). Later we can create a custom
	// client with specific timeouts for upstream API calls.
	googleCfg := cfg.Providers["google"]
	google := provider.NewGoogleProvider(
		googleCfg.APIKey,
		googleCfg.BaseURL,
		http.DefaultClient,
	)

	// Create our server with the Google provider.
	srv := server.New(cfg, google)

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      srv,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	log.Printf("llmrouter listening on :%d", cfg.Server.Port)

	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
