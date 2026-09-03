package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gateway.hooks_token is a secret, so it belongs in .security.yml next to the
// channel tokens and model keys. Before the yaml tags on GatewayConfig it was
// silently ignored there, and SaveConfig dropped it from config.json (secure
// fields never serialize to JSON) — so any onboard/auth/mcp save disabled
// /hooks/session-note without a trace.
func TestGatewayHooksToken_LoadsFromSecurityYAML(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
  "version": 2,
  "gateway": {"host": "127.0.0.1", "port": 18790, "hot_reload": true}
}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, SecurityConfigFile), []byte(
		"gateway:\n  hooks_token: \"hooks-secret\"\n"), 0o600))

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)

	assert.Equal(t, "hooks-secret", cfg.Gateway.HooksToken.String())
	// The non-secret gateway fields come from config.json and must survive the
	// .security.yml merge untouched.
	assert.Equal(t, "127.0.0.1", cfg.Gateway.Host)
	assert.Equal(t, 18790, cfg.Gateway.Port)
	assert.True(t, cfg.Gateway.HotReload)
}

func TestGatewayHooksToken_SecurityYAMLWinsOverConfigJSON(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
  "version": 2,
  "gateway": {"host": "127.0.0.1", "port": 18790, "hooks_token": "from-json"}
}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, SecurityConfigFile), []byte(
		"gateway:\n  hooks_token: \"from-yaml\"\n"), 0o600))

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "from-yaml", cfg.Gateway.HooksToken.String())
}

func TestGatewayHooksToken_SurvivesSaveConfig(t *testing.T) {
	mustSetupSSHKey(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	securityPath := filepath.Join(dir, SecurityConfigFile)
	require.NoError(t, os.WriteFile(configPath, []byte(`{
  "version": 2,
  "gateway": {"host": "127.0.0.1", "port": 18790}
}`), 0o600))
	require.NoError(t, os.WriteFile(securityPath, []byte(
		"gateway:\n  hooks_token: \"hooks-secret\"\n"), 0o600))

	cfg, err := LoadConfig(configPath)
	require.NoError(t, err)
	require.Equal(t, "hooks-secret", cfg.Gateway.HooksToken.String())

	// The save path every CLI mutation goes through (onboard, auth, mcp).
	require.NoError(t, SaveConfig(configPath, cfg))

	jsonData, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(jsonData), "hooks-secret", "secret must not land in config.json")

	yamlData, err := os.ReadFile(securityPath)
	require.NoError(t, err)
	assert.Contains(t, string(yamlData), "gateway:", "gateway block must be written to .security.yml")
	assert.Contains(t, string(yamlData), "hooks_token:")
	// Non-secret gateway fields never leak into the secrets file.
	assert.NotContains(t, string(yamlData), "host:")
	assert.NotContains(t, string(yamlData), "port:")

	reloaded, err := LoadConfig(configPath)
	require.NoError(t, err)
	assert.Equal(t, "hooks-secret", reloaded.Gateway.HooksToken.String())
	assert.Equal(t, "127.0.0.1", reloaded.Gateway.Host)
	assert.Equal(t, 18790, reloaded.Gateway.Port)
}

func TestGatewayHooksToken_EmptyOmitsGatewayBlockFromSecurityYAML(t *testing.T) {
	mustSetupSSHKey(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	require.NoError(t, SaveConfig(configPath, cfg))

	yamlData, err := os.ReadFile(filepath.Join(dir, SecurityConfigFile))
	require.NoError(t, err)
	for _, line := range strings.Split(string(yamlData), "\n") {
		assert.False(t, strings.HasPrefix(line, "gateway:"), "no secret → no gateway block, got %q", line)
	}
}
