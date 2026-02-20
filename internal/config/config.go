// Package config handles loading and validating gateway configuration.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config is the top-level configuration for the llmrouter gateway.
type Config struct {
	Server    ServerConfig              `koanf:"server"`
	Providers map[string]ProviderConfig `koanf:"providers"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port         int           `koanf:"port"`
	ReadTimeout  time.Duration `koanf:"read_timeout"`
	WriteTimeout time.Duration `koanf:"write_timeout"`
}

// ProviderConfig holds the settings for a single LLM provider.
type ProviderConfig struct {
	APIKey  string   `koanf:"api_key"`
	BaseURL string   `koanf:"base_url"`
	Models  []string `koanf:"models"`
}

// Load reads configuration from a YAML file, layers environment variable
// overrides on top, and returns a fully populated Config.
func Load(path string) (*Config, error) {
	// Load .env file into the process environment (ignored if not present).
	// This is the equivalent of require('dotenv').config() in Node.
	_ = godotenv.Load()

	// Create a new koanf instance. The "." delimiter tells koanf how to
	// separate nested keys internally (e.g., "server.port").
	k := koanf.New(".")

	// Load the YAML config file. file.Provider reads the file,
	// yaml.Parser() decodes the YAML format into koanf's internal map.
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("loading config file: %w", err)
	}

	// Layer environment variables on top. Any env var starting with
	// "LLMROUTER_" can override a config value. The callback transforms
	// the env var name into a koanf key path:
	//   LLMROUTER_SERVER_PORT -> server.port
	if err := k.Load(env.Provider("LLMROUTER_", ".", func(s string) string {
		return strings.ReplaceAll(
			strings.ToLower(strings.TrimPrefix(s, "LLMROUTER_")),
			"_", ".",
		)
	}), nil); err != nil {
		return nil, fmt.Errorf("loading env vars: %w", err)
	}

	// Unmarshal the loaded key-value pairs into our Config struct.
	// The "" means start from the root. &cfg passes a pointer so koanf
	// can write into the struct (like passing by reference in Node).
	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}

	// Expand ${VAR_NAME} placeholders in provider API keys.
	// koanf doesn't do this automatically, so we handle it ourselves
	// using os.Getenv to look up the actual environment variable value.
	for name, p := range cfg.Providers {
		if strings.HasPrefix(p.APIKey, "${") && strings.HasSuffix(p.APIKey, "}") {
			envVar := p.APIKey[2 : len(p.APIKey)-1] // strip ${ and }
			p.APIKey = os.Getenv(envVar)
			cfg.Providers[name] = p // write back into the map
		}
	}

	return &cfg, nil
}
