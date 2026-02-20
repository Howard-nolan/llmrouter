package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	// Create a temporary YAML config file with known values.
	// t.TempDir() gives us a directory that's auto-deleted after the test.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	yamlContent := `
server:
  port: 9090
  read_timeout: 10s
  write_timeout: 60s

providers:
  google:
    api_key: ${TEST_API_KEY}
    base_url: https://example.com/v1
    models:
      - model-a
      - model-b
`
	// os.WriteFile writes a byte slice to a file. The 0644 is the Unix file
	// permission (owner read/write, group and others read-only).
	err := os.WriteFile(configPath, []byte(yamlContent), 0644)
	require.NoError(t, err) // require stops the test immediately if this fails

	// Set the environment variable that ${TEST_API_KEY} should resolve to.
	// t.Setenv auto-restores the original value when the test finishes.
	t.Setenv("TEST_API_KEY", "my-secret-key")

	// Load the config.
	cfg, err := Load(configPath)
	require.NoError(t, err)

	// Assert server config values.
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, 10*time.Second, cfg.Server.ReadTimeout)
	assert.Equal(t, 60*time.Second, cfg.Server.WriteTimeout)

	// Assert provider config values.
	google, ok := cfg.Providers["google"]
	assert.True(t, ok, "google provider should exist")
	assert.Equal(t, "my-secret-key", google.APIKey)
	assert.Equal(t, "https://example.com/v1", google.BaseURL)
	assert.Equal(t, []string{"model-a", "model-b"}, google.Models)
}

func TestLoadEnvOverride(t *testing.T) {
	// Verify that LLMROUTER_ env vars override YAML values.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	yamlContent := `
server:
  port: 8080
  read_timeout: 30s
  write_timeout: 120s
`
	err := os.WriteFile(configPath, []byte(yamlContent), 0644)
	require.NoError(t, err)

	// This should override server.port from 8080 to 3000.
	t.Setenv("LLMROUTER_SERVER_PORT", "3000")

	cfg, err := Load(configPath)
	require.NoError(t, err)

	assert.Equal(t, 3000, cfg.Server.Port)
}
