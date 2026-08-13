package config

import "strings"

const (
	defaultIntakeHookTimeoutMS = 15000
	defaultIntakeHookQueueSize = 64
)

// IntakeHookConfig configures the pre-agent intake hook.
//
// picoclaw claims one turn per session, so a burst of inbound messages is
// merged into whatever turn is already running (steering) and collapses into a
// single reply. For attachment intake — filing a book cover, a receipt, a scan
// — that is the wrong shape: each attachment is an independent unit of work,
// order matters, and none of it needs the LLM.
//
// When enabled, matching messages are POSTed to URL *before* a turn is claimed,
// one at a time in arrival order. The endpoint answers
//
//	{"handled": true, "reply": "queued for check-in (#96531)"}
//
// to take ownership — the reply is posted verbatim and no turn runs — or
//
//	{"handled": false}
//
// to hand the message back to the normal agent path. Transport errors, refusals
// and a full queue all fall back to the agent, so a dead endpoint degrades to
// today's behaviour instead of swallowing the sender's message.
type IntakeHookConfig struct {
	Enabled bool         `json:"enabled"        env:"PICOCLAW_INTAKE_HOOK_ENABLED"`
	URL     string       `json:"url,omitempty"  env:"PICOCLAW_INTAKE_HOOK_URL"`
	Token   SecureString `json:"token,omitzero" env:"PICOCLAW_INTAKE_HOOK_TOKEN"`

	// TimeoutMS bounds a single POST (default 15000).
	TimeoutMS int `json:"timeout_ms,omitempty" env:"PICOCLAW_INTAKE_HOOK_TIMEOUT_MS"`
	// QueueSize bounds messages waiting for the intake worker (default 64).
	// Overflow falls back to the agent rather than dropping.
	QueueSize int `json:"queue_size,omitempty" env:"PICOCLAW_INTAKE_HOOK_QUEUE_SIZE"`
	// Channels restricts interception to these channel names (empty = all).
	Channels []string `json:"channels,omitempty"`
	// TextOnly lets messages with no attachment reach the hook too. Off by
	// default: intake is about attachments, and routing plain conversation
	// through an external endpoint would take the agent out of its own chat.
	TextOnly bool `json:"text_only,omitempty" env:"PICOCLAW_INTAKE_HOOK_TEXT_ONLY"`
}

// IsActive reports whether the hook should be consulted at all.
func (c IntakeHookConfig) IsActive() bool {
	return c.Enabled && strings.TrimSpace(c.URL) != ""
}

// EffectiveTimeoutMS returns the per-request timeout, applying the default.
func (c IntakeHookConfig) EffectiveTimeoutMS() int {
	if c.TimeoutMS > 0 {
		return c.TimeoutMS
	}
	return defaultIntakeHookTimeoutMS
}

// EffectiveQueueSize returns the intake queue depth, applying the default.
func (c IntakeHookConfig) EffectiveQueueSize() int {
	if c.QueueSize > 0 {
		return c.QueueSize
	}
	return defaultIntakeHookQueueSize
}

// MatchesChannel reports whether a channel is in scope for the hook.
// An empty Channels list means every channel is in scope.
func (c IntakeHookConfig) MatchesChannel(name string) bool {
	if len(c.Channels) == 0 {
		return true
	}
	name = strings.TrimSpace(strings.ToLower(name))
	for _, candidate := range c.Channels {
		if strings.TrimSpace(strings.ToLower(candidate)) == name {
			return true
		}
	}
	return false
}
