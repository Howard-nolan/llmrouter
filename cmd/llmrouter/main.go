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

	// Build the provider registry: a map from model name → Provider.
	//
	// We create each provider based on what's in the config, then
	// register every model that provider supports. This way the
	// handler can do a single map lookup to find the right provider
	// for any model name.
	//
	// providerConstructors maps provider names (from config) to the
	// function that creates them. This avoids a big if/else chain
	// and makes it easy to add new providers later — just add an
	// entry here.
	//
	// The map value type is a function: func(apiKey, baseURL string) provider.Provider
	// This is a common Go pattern for factory functions — you store
	// the constructor in the map so you can call it later with the
	// right config values. It's like a Map<string, (key, url) => Provider>
	// in TypeScript.
	type providerFactory func(apiKey, baseURL string) provider.Provider

	constructors := map[string]providerFactory{
		"google": func(apiKey, baseURL string) provider.Provider {
			return provider.NewGoogleProvider(apiKey, baseURL, http.DefaultClient)
		},
		"anthropic": func(apiKey, baseURL string) provider.Provider {
			return provider.NewAnthropicProvider(apiKey, baseURL, http.DefaultClient)
		},
	}

	// Iterate the providers from config and register each model.
	models := make(map[string]provider.Provider)

	for name, provCfg := range cfg.Providers {
		factory, ok := constructors[name]
		if !ok {
			log.Fatalf("unknown provider in config: %q", name)
		}

		p := factory(provCfg.APIKey, provCfg.BaseURL)

		for _, model := range provCfg.Models {
			models[model] = p
			log.Printf("registered model %q → provider %q", model, name)
		}
	}

	srv := server.New(cfg, models)

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
