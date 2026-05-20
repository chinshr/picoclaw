package voice

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/bus"
)

// voiceStreamer pushes incremental LLM output to a voice client.
//
// It differs from picoStreamer in one important way: when sentenceFlush is
// enabled, every time a new sentence-ending punctuation appears in the
// content, the streamer flushes immediately, bypassing the throttle window.
// This lets the downstream edge device start TTS on a complete sentence as
// soon as it's available, rather than waiting up to throttleInterval before
// receiving it.
//
// Between sentence boundaries the streamer still honors throttleInterval and
// minGrowth, so we don't spam the wire with single-token updates.
type voiceStreamer struct {
	channel          *VoiceChannel
	chatID           string
	messageID        string
	reasoningID      string
	throttleInterval time.Duration
	minGrowth        int
	sentenceFlush    bool

	mu               sync.Mutex
	lastLen          int
	lastAt           time.Time
	lastContent      string
	lastSentenceEnd  int // rune index of the most recent sentence-end already flushed
	reasoningLastLen int
	reasoningLastAt  time.Time
	reasoningContent string
}

// Sentence-ending punctuation. Covers ASCII (.!?) and CJK fullwidth forms.
// Newlines also act as a flush boundary — paragraph breaks are natural seams.
const sentenceEnders = ".!?。！？\n"

// lastSentenceEndIndex returns the rune index of the last sentence-ending
// character in content (counting in runes, not bytes). Returns -1 if none.
//
// It deliberately treats every "." as a boundary, including those in decimals.
// In streaming utterance mode the cost of an extra sentence flush is small;
// the cost of waiting on "3.14" indefinitely is much worse for perceived
// latency. If clients care, they can disable sentenceFlush.
func lastSentenceEndIndex(content string) int {
	runes := []rune(content)
	for i := len(runes) - 1; i >= 0; i-- {
		if strings.ContainsRune(sentenceEnders, runes[i]) {
			return i
		}
	}
	return -1
}

func (s *voiceStreamer) Update(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(ctx, content, false, nil)
}

func (s *voiceStreamer) Finalize(ctx context.Context, content string) error {
	return s.FinalizeWithContext(ctx, content, nil)
}

func (s *voiceStreamer) FinalizeWithContext(
	ctx context.Context,
	content string,
	contextUsage *bus.ContextUsage,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(ctx, content, true, contextUsage)
}

func (s *voiceStreamer) UpdateReasoning(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateReasoningLocked(ctx, content, false)
}

func (s *voiceStreamer) FinalizeReasoning(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateReasoningLocked(ctx, content, true)
}

func (s *voiceStreamer) Cancel(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channel != nil && s.messageID != "" {
		_ = s.channel.DeleteMessage(ctx, s.chatID, s.messageID)
		s.messageID = ""
	}
	if s.channel != nil && s.reasoningID != "" {
		_ = s.channel.DeleteMessage(ctx, s.chatID, s.reasoningID)
		s.reasoningID = ""
	}
}

func (s *voiceStreamer) updateLocked(
	ctx context.Context,
	content string,
	final bool,
	contextUsage *bus.ContextUsage,
) error {
	if s == nil || s.channel == nil {
		return fmt.Errorf("voice streamer not initialized")
	}
	if strings.TrimSpace(content) == "" && s.messageID == "" && !final {
		return nil
	}

	contentLen := len([]rune(content))
	now := time.Now()

	if s.messageID != "" && !final {
		// Check if a new sentence has completed since the last flush — if so,
		// flush eagerly regardless of throttle/min-growth.
		if s.sentenceFlush {
			if end := lastSentenceEndIndex(content); end > s.lastSentenceEnd {
				return s.sendLocked(ctx, content, contextUsage, final, end)
			}
		}
		// Otherwise apply the normal throttle/min-growth filter.
		growth := contentLen - s.lastLen
		if now.Sub(s.lastAt) < s.throttleInterval || growth < s.minGrowth {
			return nil
		}
	}

	end := -1
	if s.sentenceFlush {
		end = lastSentenceEndIndex(content)
	}
	return s.sendLocked(ctx, content, contextUsage, final, end)
}

func (s *voiceStreamer) sendLocked(
	ctx context.Context,
	content string,
	contextUsage *bus.ContextUsage,
	final bool,
	sentenceEnd int,
) error {
	now := time.Now()
	contentLen := len([]rune(content))

	if s.messageID == "" {
		s.messageID = uuid.New().String()
		payload := map[string]any{
			PayloadKeyContent: content,
			"message_id":      s.messageID,
		}
		if final {
			payload[PayloadKeyFinal] = true
		}
		setContextUsagePayload(payload, contextUsage)
		outMsg := newMessage(TypeMessageCreate, payload)
		if err := s.channel.broadcastToSession(s.chatID, outMsg); err != nil {
			return err
		}
	} else if content != s.lastContent || contextUsage != nil || final {
		if err := s.channel.editMessage(ctx, s.chatID, s.messageID, content, contextUsage, final); err != nil {
			return err
		}
	}

	s.lastContent = content
	s.lastLen = contentLen
	s.lastAt = now
	if sentenceEnd >= 0 {
		s.lastSentenceEnd = sentenceEnd
	}
	return nil
}

func (s *voiceStreamer) updateReasoningLocked(
	ctx context.Context,
	content string,
	final bool,
) error {
	if s == nil || s.channel == nil {
		return fmt.Errorf("voice streamer not initialized")
	}
	if strings.TrimSpace(content) == "" && s.reasoningID == "" {
		return nil
	}

	contentLen := len([]rune(content))
	now := time.Now()
	if s.reasoningID != "" && !final {
		growth := contentLen - s.reasoningLastLen
		if now.Sub(s.reasoningLastAt) < s.throttleInterval || growth < s.minGrowth {
			return nil
		}
	}

	return s.sendReasoningLocked(ctx, content)
}

func (s *voiceStreamer) sendReasoningLocked(ctx context.Context, content string) error {
	now := time.Now()
	contentLen := len([]rune(content))

	if s.reasoningID == "" {
		s.reasoningID = uuid.New().String()
		payload := map[string]any{
			PayloadKeyContent: content,
			"message_id":      s.reasoningID,
			PayloadKeyKind:    MessageKindThought,
		}
		outMsg := newMessage(TypeMessageCreate, payload)
		if err := s.channel.broadcastToSession(s.chatID, outMsg); err != nil {
			return err
		}
	} else if content != s.reasoningContent {
		payload := map[string]any{
			PayloadKeyContent: content,
			"message_id":      s.reasoningID,
			PayloadKeyKind:    MessageKindThought,
		}
		outMsg := newMessage(TypeMessageUpdate, payload)
		if err := s.channel.broadcastToSession(s.chatID, outMsg); err != nil {
			return err
		}
	}

	s.reasoningContent = content
	s.reasoningLastLen = contentLen
	s.reasoningLastAt = now
	return nil
}

// setContextUsagePayload mirrors the pico helper of the same name.
func setContextUsagePayload(payload map[string]any, u *bus.ContextUsage) {
	if u == nil {
		return
	}
	payload["context_usage"] = map[string]any{
		"used_tokens":        u.UsedTokens,
		"total_tokens":       u.TotalTokens,
		"compress_at_tokens": u.CompressAtTokens,
		"used_percent":       u.UsedPercent,
	}
}
