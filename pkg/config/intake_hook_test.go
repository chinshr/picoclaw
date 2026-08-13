package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestIntakeHookConfig_IsActiveRequiresURL(t *testing.T) {
	if (IntakeHookConfig{Enabled: true}).IsActive() {
		t.Error("an enabled hook with no URL must stay inactive")
	}
	if (IntakeHookConfig{URL: "http://127.0.0.1:1/x"}).IsActive() {
		t.Error("a URL alone must not activate the hook")
	}
	if !(IntakeHookConfig{Enabled: true, URL: "http://127.0.0.1:1/x"}).IsActive() {
		t.Error("enabled + URL must be active")
	}
}

func TestIntakeHookConfig_Defaults(t *testing.T) {
	var cfg IntakeHookConfig
	if got := cfg.EffectiveTimeoutMS(); got != defaultIntakeHookTimeoutMS {
		t.Errorf("timeout = %d, want %d", got, defaultIntakeHookTimeoutMS)
	}
	if got := cfg.EffectiveQueueSize(); got != defaultIntakeHookQueueSize {
		t.Errorf("queue size = %d, want %d", got, defaultIntakeHookQueueSize)
	}
	cfg.TimeoutMS, cfg.QueueSize = 250, 4
	if cfg.EffectiveTimeoutMS() != 250 || cfg.EffectiveQueueSize() != 4 {
		t.Error("explicit values must win over defaults")
	}
}

func TestIntakeHookConfig_MatchesChannel(t *testing.T) {
	empty := IntakeHookConfig{}
	if !empty.MatchesChannel("discord") {
		t.Error("an empty channel list means every channel")
	}
	only := IntakeHookConfig{Channels: []string{"Telegram"}}
	if !only.MatchesChannel("telegram") {
		t.Error("channel matching must be case-insensitive")
	}
	if only.MatchesChannel("discord") {
		t.Error("channels outside the list must not be intercepted")
	}
}

func TestConfigExample_ParsesIntakeHook(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "config.example.json"))
	if err != nil {
		t.Fatalf("ReadFile(config.example.json) error: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("Unmarshal(config.example.json) error: %v", err)
	}
	intake := cfg.Hooks.Intake
	if intake.Enabled {
		t.Error("the example must ship with intake off")
	}
	if intake.URL == "" {
		t.Error("the example must show the endpoint URL")
	}
	if len(intake.Channels) != 1 || intake.Channels[0] != "telegram" {
		t.Errorf("example channels = %v, want [telegram]", intake.Channels)
	}
}
