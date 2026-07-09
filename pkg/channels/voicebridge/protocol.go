package voicebridge

import "time"

// Protocol message types. The wire format intentionally mirrors the Pico
// Protocol so that bridge/edge tooling that already speaks the Pico envelope
// can talk to the voice channel with only a different webhook path. The
// channel-tag in the envelope ("voice_bridge") is what differentiates the two on the
// wire, alongside the path the client connects to.
const (
	// Inbound (client → server).
	TypeMessageSend = "message.send"
	TypePing        = "ping"

	// Speculative turns (docs/design/speculative-turns.md). The bridge connects
	// to THIS channel (/voice_bridge/ws), so the speculative protocol lives here.
	TypeTurnCommit = "turn.commit"
	TypeTurnAbort  = "turn.abort"

	// Outbound (server → client).
	TypeMessageCreate = "message.create"
	TypeMessageUpdate = "message.update"
	TypeMessageDelete = "message.delete"
	TypeTypingStart   = "typing.start"
	TypeTypingStop    = "typing.stop"
	TypeError         = "error"
	TypePong          = "pong"

	PayloadKeyContent     = "content"
	PayloadKeyPlaceholder = "placeholder"
	PayloadKeyKind        = "kind"
	PayloadKeyFinal       = "final"

	// Speculative-turn payload keys (Raw-metadata keys live in pkg/bus).
	PayloadKeySpeculative   = "speculative"
	PayloadKeySpeculationID = "speculation_id"

	MessageKindThought = "thought"
)

// VoiceMessage is the wire format for all voice channel messages.
type VoiceMessage struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Timestamp int64          `json:"timestamp,omitempty"`
	Payload   map[string]any `json:"payload,omitempty"`
}

// newMessage builds a VoiceMessage with the given type and payload, stamping
// the current time in milliseconds.
func newMessage(msgType string, payload map[string]any) VoiceMessage {
	return VoiceMessage{
		Type:      msgType,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	}
}

// newError builds a typed error envelope. extra fields are merged into the
// payload (e.g. correlating request_id back to the client).
func newError(code, message string, extra map[string]any) VoiceMessage {
	payload := map[string]any{
		"code":    code,
		"message": message,
	}
	for k, v := range extra {
		payload[k] = v
	}
	return newMessage(TypeError, payload)
}
