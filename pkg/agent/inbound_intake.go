package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// RawKeyIntakeSeen marks a message the intake hook has already been offered,
// so one handed back by the endpoint is never offered to it twice.
const RawKeyIntakeSeen = "pico_intake_seen"

// intakeAttachment is one resolved attachment as the endpoint sees it.
// Path is the local file the endpoint can read directly; Missing marks a ref
// whose file is gone (TTL sweep, restart) so the endpoint can say so plainly
// instead of filing a book nobody can see.
type intakeAttachment struct {
	Ref      string `json:"ref"`
	Path     string `json:"path,omitempty"`
	Filename string `json:"filename,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Missing  bool   `json:"missing,omitempty"`
}

type intakeRequest struct {
	Channel     string             `json:"channel"`
	ChatID      string             `json:"chat_id"`
	MessageID   string             `json:"message_id,omitempty"`
	SenderID    string             `json:"sender_id,omitempty"`
	SenderName  string             `json:"sender_name,omitempty"`
	SessionKey  string             `json:"session_key,omitempty"`
	Text        string             `json:"text"`
	Attachments []intakeAttachment `json:"attachments"`
}

type intakeResponse struct {
	Handled bool   `json:"handled"`
	Reply   string `json:"reply,omitempty"`
	Error   string `json:"error,omitempty"`
}

// intakeDispatcher runs the pre-agent intake hook.
//
// Messages are handled by a single worker goroutine: intake is an ordered
// pipeline (the receiving side files attachments in submission order), and the
// worker keeps the inbound bus loop free while a POST is in flight.
type intakeDispatcher struct {
	al     *AgentLoop
	client *http.Client
	queue  chan bus.InboundMessage

	startOnce sync.Once
	stopOnce  sync.Once
	done      chan struct{}
}

// newIntakeDispatcher builds the dispatcher regardless of whether the hook is
// currently enabled: enablement, URL, token and timeout are read live from the
// loop's config on every message, so a hot reload that turns the hook on (or
// repoints it) takes effect without a restart. Only the queue depth is fixed
// at construction.
func newIntakeDispatcher(al *AgentLoop) *intakeDispatcher {
	if al == nil {
		return nil
	}
	return &intakeDispatcher{
		al:     al,
		client: &http.Client{}, // per-request deadline comes from the context
		queue:  make(chan bus.InboundMessage, al.intakeHookConfig().EffectiveQueueSize()),
		done:   make(chan struct{}),
	}
}

// config returns the live intake configuration.
func (d *intakeDispatcher) config() config.IntakeHookConfig {
	if d == nil || d.al == nil {
		return config.IntakeHookConfig{}
	}
	return d.al.intakeHookConfig()
}

// ShouldHandle reports whether a message is a candidate for the intake hook.
func (d *intakeDispatcher) ShouldHandle(msg bus.InboundMessage) bool {
	if d == nil {
		return false
	}
	cfg := d.config()
	if !cfg.IsActive() {
		return false
	}
	if msg.Context.Raw[RawKeyIntakeSeen] != "" {
		return false
	}
	if !cfg.MatchesChannel(msg.Channel) {
		return false
	}
	if len(msg.Media) == 0 && !cfg.TextOnly {
		return false
	}
	return true
}

// Enqueue hands a message to the intake worker. It never blocks: a full queue
// returns false and the caller keeps the normal agent path, because a slow
// endpoint must not be able to stall the inbound loop or lose a message.
func (d *intakeDispatcher) Enqueue(msg bus.InboundMessage) bool {
	if d == nil {
		return false
	}
	select {
	case d.queue <- msg:
		return true
	default:
		logger.WarnCF("agent", "Intake queue full, falling back to agent turn",
			map[string]any{
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
				"depth":   cap(d.queue),
			})
		return false
	}
}

// Start launches the worker once.
func (d *intakeDispatcher) Start(ctx context.Context) {
	if d == nil {
		return
	}
	d.startOnce.Do(func() {
		go d.run(ctx)
	})
}

func (d *intakeDispatcher) run(ctx context.Context) {
	defer d.stopOnce.Do(func() { close(d.done) })
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-d.queue:
			if !ok {
				return
			}
			d.handle(ctx, msg)
		}
	}
}

func (d *intakeDispatcher) handle(ctx context.Context, msg bus.InboundMessage) {
	req := d.buildRequest(msg)
	resp, err := d.post(ctx, req)
	if err != nil {
		logger.WarnCF("agent", "Intake hook failed, falling back to agent turn",
			map[string]any{
				"error":   err.Error(),
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
			})
		d.fallback(ctx, msg)
		return
	}
	if !resp.Handled {
		logger.InfoCF("agent", "Intake hook declined message",
			map[string]any{
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
				"reason":  resp.Error,
			})
		d.fallback(ctx, msg)
		return
	}

	logger.InfoCF("agent", "Intake hook handled message",
		map[string]any{
			"channel":     msg.Channel,
			"chat_id":     msg.ChatID,
			"message_id":  msg.Context.MessageID,
			"attachments": len(req.Attachments),
			"replied":     strings.TrimSpace(resp.Reply) != "",
		})

	reply := strings.TrimSpace(resp.Reply)
	if reply == "" {
		return
	}
	// Same shape a turn's final reply has, so the channel resolves the
	// "Thinking…" placeholder this message triggered instead of stranding it.
	inboundCtx := msg.Context
	outbound := bus.OutboundMessage{
		Channel:    msg.Channel,
		ChatID:     msg.ChatID,
		Context:    outboundContextFromInbound(&inboundCtx, msg.Channel, msg.ChatID, msg.Context.MessageID),
		SessionKey: msg.SessionKey,
		Content:    reply,
	}
	markFinalOutbound(&outbound)
	if err := d.al.bus.PublishOutbound(ctx, outbound); err != nil {
		logger.WarnCF("agent", "Failed to publish intake reply",
			map[string]any{
				"error":   err.Error(),
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
			})
	}
}

// fallback returns a message the hook did not take to the normal agent path,
// marked so the hook is not offered it a second time.
func (d *intakeDispatcher) fallback(ctx context.Context, msg bus.InboundMessage) {
	raw := make(map[string]string, len(msg.Context.Raw)+1)
	for k, v := range msg.Context.Raw {
		raw[k] = v
	}
	raw[RawKeyIntakeSeen] = "1"
	msg.Context.Raw = raw

	if err := d.al.bus.PublishInbound(ctx, msg); err != nil {
		logger.WarnCF("agent", "Failed to return declined intake message to the agent",
			map[string]any{
				"error":   err.Error(),
				"channel": msg.Channel,
				"chat_id": msg.ChatID,
			})
	}
}

func (d *intakeDispatcher) buildRequest(msg bus.InboundMessage) intakeRequest {
	req := intakeRequest{
		Channel:    msg.Channel,
		ChatID:     msg.ChatID,
		MessageID:  msg.Context.MessageID,
		SenderID:   msg.SenderID,
		SenderName: msg.Sender.DisplayName,
		SessionKey: msg.SessionKey,
		Text:       msg.Content,
	}
	store := d.al.mediaStore
	for _, ref := range msg.Media {
		att := intakeAttachment{Ref: ref}
		if store == nil {
			att.Missing = true
			req.Attachments = append(req.Attachments, att)
			continue
		}
		localPath, meta, err := store.ResolveWithMeta(ref)
		if err != nil {
			att.Missing = true
			req.Attachments = append(req.Attachments, att)
			continue
		}
		info, err := os.Stat(localPath)
		if err != nil {
			att.Path = localPath
			att.Missing = true
			req.Attachments = append(req.Attachments, att)
			continue
		}
		att.Path = localPath
		att.Filename = meta.Filename
		att.MIME = detectMIME(localPath, meta)
		att.Bytes = info.Size()
		req.Attachments = append(req.Attachments, att)
	}
	return req
}

func (d *intakeDispatcher) post(ctx context.Context, payload intakeRequest) (intakeResponse, error) {
	var out intakeResponse

	body, err := json.Marshal(payload)
	if err != nil {
		return out, fmt.Errorf("marshal intake request: %w", err)
	}

	cfg := d.config()
	timeout := time.Duration(cfg.EffectiveTimeoutMS()) * time.Millisecond
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("build intake request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(cfg.Token.String()); token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("X-Intake-Token", token)
	}

	httpResp, err := d.client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("read intake response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return out, fmt.Errorf("intake endpoint returned %d: %s",
			httpResp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode intake response: %w", err)
	}
	return out, nil
}

// intakeHookConfig returns the live intake hook configuration. It is read under
// the loop lock because ReloadConfig swaps al.cfg wholesale.
func (al *AgentLoop) intakeHookConfig() config.IntakeHookConfig {
	if al == nil {
		return config.IntakeHookConfig{}
	}
	al.mu.RLock()
	defer al.mu.RUnlock()
	if al.cfg == nil {
		return config.IntakeHookConfig{}
	}
	return al.cfg.Hooks.Intake
}
