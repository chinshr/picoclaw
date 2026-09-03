package config

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/netbind"
)

const DefaultGatewayLogLevel = "warn"

// GatewayConfig's only secret is HooksToken, so it is the only field that
// round-trips through .security.yml (yaml tags below). The non-secret fields
// stay in config.json. Without this split the token had nowhere durable to
// live: SecureString never serializes to config.json, and the whole gateway
// block used to be excluded from .security.yml, so any SaveConfig (onboard,
// auth, mcp) silently dropped it and /hooks/session-note went dark.
type GatewayConfig struct {
	Host      string `json:"host"                yaml:"-" env:"PICOCLAW_GATEWAY_HOST"`
	Port      int    `json:"port"                yaml:"-" env:"PICOCLAW_GATEWAY_PORT"`
	HotReload bool   `json:"hot_reload"          yaml:"-" env:"PICOCLAW_GATEWAY_HOT_RELOAD"`
	LogLevel  string `json:"log_level,omitempty" yaml:"-" env:"PICOCLAW_LOG_LEVEL"`
	// HooksToken authorizes out-of-band POSTs to /hooks/session-note (workers
	// mirroring their platform-posted lines into session history). Empty
	// disables the endpoint. Supports enc:// / file:// refs like other secrets.
	// Lives in .security.yml under gateway.hooks_token (or the env var).
	HooksToken SecureString `json:"hooks_token,omitzero" yaml:"hooks_token,omitempty" env:"PICOCLAW_HOOKS_TOKEN"`
}

// IsZero lets yaml's omitempty drop the whole gateway block from
// .security.yml when there is no secret to store, instead of writing
// "gateway: {}". Only consulted by the yaml encoder (json has no omitzero on
// the Gateway field). Deliberately not SecureString.IsZero: that one inspects
// its caller's file to tell yaml from json, and from here the caller is this
// method, not the yaml encoder, so it would always report zero.
func (g GatewayConfig) IsZero() bool {
	return g.HooksToken.String() == ""
}

func canonicalGatewayLogLevel(level logger.LogLevel) string {
	switch level {
	case logger.DEBUG:
		return "debug"
	case logger.INFO:
		return "info"
	case logger.WARN:
		return "warn"
	case logger.ERROR:
		return "error"
	case logger.FATAL:
		return "fatal"
	default:
		return DefaultGatewayLogLevel
	}
}

func normalizeGatewayLogLevel(logLevel string) string {
	if level, ok := logger.ParseLevel(logLevel); ok {
		return canonicalGatewayLogLevel(level)
	}
	return DefaultGatewayLogLevel
}

// EffectiveGatewayLogLevel returns the normalized runtime log level from a loaded config.
// Invalid or empty values fall back to the package default.
func EffectiveGatewayLogLevel(cfg *Config) string {
	if cfg == nil {
		return DefaultGatewayLogLevel
	}
	return normalizeGatewayLogLevel(cfg.Gateway.LogLevel)
}

func resolveGatewayHostFromEnv(baseHost string) (string, error) {
	envHost, ok := os.LookupEnv(EnvGatewayHost)
	if !ok {
		return normalizeGatewayHostInput(baseHost)
	}

	envHost = strings.TrimSpace(envHost)
	if envHost == "" {
		return normalizeGatewayHostInput(baseHost)
	}

	return normalizeGatewayHostInput(envHost)
}

func normalizeGatewayHostInput(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = strings.TrimSpace(DefaultConfig().Gateway.Host)
	}
	if host == "" {
		host = "localhost"
	}
	return netbind.NormalizeHostInput(host)
}

// ResolveGatewayLogLevel reads the configured gateway log level without triggering
// the full config loader, so startup code can apply logging before config load logs run.
// The PICOCLAW_LOG_LEVEL environment variable overrides the file value.
func ResolveGatewayLogLevel(path string) string {
	cfg := struct {
		Gateway GatewayConfig `json:"gateway"`
	}{
		Gateway: GatewayConfig{LogLevel: DefaultGatewayLogLevel},
	}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			logger.WarnCF("config", "failed to parse gateway config, using defaults", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
		}
	}

	if envLevel := os.Getenv("PICOCLAW_LOG_LEVEL"); envLevel != "" {
		cfg.Gateway.LogLevel = envLevel
	}

	return normalizeGatewayLogLevel(cfg.Gateway.LogLevel)
}
