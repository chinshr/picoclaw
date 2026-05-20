package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// voiceConn represents a single WebSocket connection to a voice client.
// It mirrors picoConn — kept private to this package so the two channels can
// evolve independently.
type voiceConn struct {
	id        string
	conn      *websocket.Conn
	sessionID string
	writeMu   sync.Mutex
	closed    atomic.Bool
	cancel    context.CancelFunc
}

func (vc *voiceConn) writeJSON(v any) error {
	if vc.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	vc.writeMu.Lock()
	defer vc.writeMu.Unlock()
	return vc.conn.WriteJSON(v)
}

func (vc *voiceConn) close() {
	if vc.closed.CompareAndSwap(false, true) {
		if vc.cancel != nil {
			vc.cancel()
		}
		vc.conn.Close()
	}
}

// VoiceChannel is a WebSocket channel modeled on PicoChannel and tuned for
// streaming partial LLM utterances to a downstream device (e.g. edge) that
// performs TTS playback. It implements channels.StreamingCapable; the streamer
// returned from BeginStream flushes early on sentence boundaries.
type VoiceChannel struct {
	*channels.BaseChannel
	bc                 *config.Channel
	config             *config.VoiceSettings
	upgrader           websocket.Upgrader
	connections        map[string]*voiceConn            // connID -> *voiceConn
	sessionConnections map[string]map[string]*voiceConn // sessionID -> connID -> *voiceConn
	connsMu            sync.RWMutex
	ctx                context.Context
	cancel             context.CancelFunc
}

// NewVoiceChannel builds a new voice channel from validated settings.
func NewVoiceChannel(
	bc *config.Channel,
	cfg *config.VoiceSettings,
	messageBus *bus.MessageBus,
) (*VoiceChannel, error) {
	if cfg.Token.String() == "" {
		return nil, fmt.Errorf("voice token is required")
	}

	base := channels.NewBaseChannel("voice", cfg, messageBus, bc.AllowFrom)

	allowOrigins := cfg.AllowOrigins
	checkOrigin := func(r *http.Request) bool {
		if len(allowOrigins) == 0 {
			return true
		}
		origin := r.Header.Get("Origin")
		for _, allowed := range allowOrigins {
			if allowed == "*" || allowed == origin {
				return true
			}
		}
		return false
	}

	ch := &VoiceChannel{
		BaseChannel: base,
		bc:          bc,
		config:      cfg,
		upgrader: websocket.Upgrader{
			CheckOrigin:     checkOrigin,
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
		},
		connections:        make(map[string]*voiceConn),
		sessionConnections: make(map[string]map[string]*voiceConn),
	}
	return ch, nil
}

// Start implements channels.Channel.
func (c *VoiceChannel) Start(ctx context.Context) error {
	logger.InfoC("voice", "Starting voice channel")
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.SetRunning(true)
	logger.InfoC("voice", "Voice channel started")
	return nil
}

// Stop implements channels.Channel.
func (c *VoiceChannel) Stop(ctx context.Context) error {
	logger.InfoC("voice", "Stopping voice channel")
	c.SetRunning(false)

	for _, vc := range c.takeAllConnections() {
		vc.close()
	}

	if c.cancel != nil {
		c.cancel()
	}

	logger.InfoC("voice", "Voice channel stopped")
	return nil
}

// WebhookPath implements channels.WebhookHandler. The voice channel sits at
// /voice/ so it can coexist with the pico channel on the same HTTP server.
func (c *VoiceChannel) WebhookPath() string { return "/voice/" }

// ServeHTTP implements http.Handler.
func (c *VoiceChannel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/voice")
	switch path {
	case "/ws", "/ws/":
		c.handleWebSocket(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Send implements channels.Channel.
func (c *VoiceChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}

	msgID := uuid.New().String()
	payload := map[string]any{
		PayloadKeyContent: msg.Content,
		"message_id":      msgID,
	}
	setContextUsagePayload(payload, msg.ContextUsage)
	outMsg := newMessage(TypeMessageCreate, payload)

	if err := c.broadcastToSession(msg.ChatID, outMsg); err != nil {
		return nil, err
	}
	return []string{msgID}, nil
}

// EditMessage implements channels.MessageEditor.
func (c *VoiceChannel) EditMessage(
	ctx context.Context,
	chatID string,
	messageID string,
	content string,
) error {
	return c.editMessage(ctx, chatID, messageID, content, nil, false)
}

func (c *VoiceChannel) editMessage(
	ctx context.Context,
	chatID string,
	messageID string,
	content string,
	contextUsage *bus.ContextUsage,
	final bool,
) error {
	payload := map[string]any{
		"message_id":      messageID,
		PayloadKeyContent: content,
	}
	if final {
		payload[PayloadKeyFinal] = true
	}
	setContextUsagePayload(payload, contextUsage)
	outMsg := newMessage(TypeMessageUpdate, payload)
	return c.broadcastToSession(chatID, outMsg)
}

// DeleteMessage implements channels.MessageDeleter.
func (c *VoiceChannel) DeleteMessage(ctx context.Context, chatID string, messageID string) error {
	outMsg := newMessage(TypeMessageDelete, map[string]any{
		"message_id": messageID,
	})
	return c.broadcastToSession(chatID, outMsg)
}

// StartTyping implements channels.TypingCapable.
func (c *VoiceChannel) StartTyping(ctx context.Context, chatID string) (func(), error) {
	startMsg := newMessage(TypeTypingStart, nil)
	if err := c.broadcastToSession(chatID, startMsg); err != nil {
		return func() {}, err
	}
	return func() {
		stopMsg := newMessage(TypeTypingStop, nil)
		_ = c.broadcastToSession(chatID, stopMsg)
	}, nil
}

// SendPlaceholder implements channels.PlaceholderCapable.
func (c *VoiceChannel) SendPlaceholder(ctx context.Context, chatID string) (string, error) {
	if !c.bc.Placeholder.Enabled {
		return "", nil
	}

	text := c.bc.Placeholder.GetRandomText()
	msgID := uuid.New().String()
	outMsg := newMessage(TypeMessageCreate, map[string]any{
		PayloadKeyContent:     text,
		PayloadKeyPlaceholder: true,
		"message_id":          msgID,
	})

	if err := c.broadcastToSession(chatID, outMsg); err != nil {
		return "", err
	}
	return msgID, nil
}

// BeginStream implements channels.StreamingCapable. Returns a voiceStreamer
// that flushes eagerly at sentence boundaries.
func (c *VoiceChannel) BeginStream(ctx context.Context, chatID string) (channels.Streamer, error) {
	if c == nil || c.config == nil || !c.config.Streaming.Enabled {
		return nil, fmt.Errorf("streaming disabled in config")
	}
	if !c.IsRunning() {
		return nil, channels.ErrNotRunning
	}
	// Voice defaults: 0s throttle, 1-rune min growth. Edge wants partial
	// output as fast as possible; the sentence-flush path will short-circuit
	// these anyway.
	streamCfg := c.config.Streaming.WithDefaults(0, 1)
	return &voiceStreamer{
		channel:          c,
		chatID:           chatID,
		throttleInterval: time.Duration(streamCfg.ThrottleSeconds) * time.Second,
		minGrowth:        streamCfg.MinGrowthChars,
		sentenceFlush:    c.config.SentenceFlushEnabled(),
		lastSentenceEnd:  -1,
	}, nil
}

// broadcastToSession sends a message to every connection in the chatID's
// session. Returns ErrSendFailed wrapped if no active connections accepted it.
func (c *VoiceChannel) broadcastToSession(chatID string, msg VoiceMessage) error {
	sessionID := strings.TrimPrefix(chatID, "voice:")
	msg.SessionID = sessionID

	var sent bool
	for _, vc := range c.sessionConnectionsSnapshot(sessionID) {
		if err := vc.writeJSON(msg); err != nil {
			logger.DebugCF("voice", "Write to connection failed", map[string]any{
				"conn_id": vc.id,
				"error":   err.Error(),
			})
		} else {
			sent = true
		}
	}

	if !sent {
		return fmt.Errorf("no active connections for session %s: %w", sessionID, channels.ErrSendFailed)
	}
	return nil
}

// createAndAddConnection registers a new connection, atomically enforcing the
// MaxConnections limit.
func (c *VoiceChannel) createAndAddConnection(
	conn *websocket.Conn,
	sessionID string,
	maxConns int,
) (*voiceConn, error) {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()
	if len(c.connections) >= maxConns {
		return nil, channels.ErrTemporary
	}

	var connID string
	for {
		connID = uuid.New().String()
		if _, exists := c.connections[connID]; !exists {
			break
		}
	}

	vc := &voiceConn{
		id:        connID,
		conn:      conn,
		sessionID: sessionID,
	}

	c.connections[vc.id] = vc
	bySession, ok := c.sessionConnections[vc.sessionID]
	if !ok {
		bySession = make(map[string]*voiceConn)
		c.sessionConnections[vc.sessionID] = bySession
	}
	bySession[vc.id] = vc

	return vc, nil
}

func (c *VoiceChannel) removeConnection(connID string) *voiceConn {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()

	vc, ok := c.connections[connID]
	if !ok {
		return nil
	}

	delete(c.connections, connID)
	if bySession, ok := c.sessionConnections[vc.sessionID]; ok {
		delete(bySession, connID)
		if len(bySession) == 0 {
			delete(c.sessionConnections, vc.sessionID)
		}
	}
	return vc
}

func (c *VoiceChannel) takeAllConnections() []*voiceConn {
	c.connsMu.Lock()
	defer c.connsMu.Unlock()

	all := make([]*voiceConn, 0, len(c.connections))
	for _, vc := range c.connections {
		all = append(all, vc)
	}
	clear(c.connections)
	clear(c.sessionConnections)
	return all
}

func (c *VoiceChannel) sessionConnectionsSnapshot(sessionID string) []*voiceConn {
	c.connsMu.RLock()
	defer c.connsMu.RUnlock()

	bySession, ok := c.sessionConnections[sessionID]
	if !ok || len(bySession) == 0 {
		return nil
	}
	conns := make([]*voiceConn, 0, len(bySession))
	for _, vc := range bySession {
		conns = append(conns, vc)
	}
	return conns
}

func (c *VoiceChannel) currentConnCount() int {
	c.connsMu.RLock()
	defer c.connsMu.RUnlock()
	return len(c.connections)
}

func (c *VoiceChannel) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !c.IsRunning() {
		http.Error(w, "channel not running", http.StatusServiceUnavailable)
		return
	}

	if !c.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	maxConns := c.config.MaxConnections
	if maxConns <= 0 {
		maxConns = 100
	}
	if c.currentConnCount() >= maxConns {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	var responseHeader http.Header
	if proto := c.matchedSubprotocol(r); proto != "" {
		responseHeader = http.Header{"Sec-WebSocket-Protocol": {proto}}
	}

	conn, err := c.upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		logger.ErrorCF("voice", "WebSocket upgrade failed", map[string]any{
			"error": err.Error(),
		})
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		sessionID = uuid.New().String()
	}

	vc, err := c.createAndAddConnection(conn, sessionID, maxConns)
	if err != nil {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many connections"),
			time.Now().Add(2*time.Second),
		)
		_ = conn.Close()
		return
	}

	logger.InfoCF("voice", "WebSocket client connected", map[string]any{
		"conn_id":    vc.id,
		"session_id": sessionID,
	})

	go c.readLoop(vc)
}

// authenticate accepts the same three credential sources as the pico channel:
//  1. Authorization: Bearer <token>
//  2. Sec-WebSocket-Protocol "token.<value>" subprotocol
//  3. ?token=<value> query string (only when AllowTokenQuery is on)
func (c *VoiceChannel) authenticate(r *http.Request) bool {
	token := c.config.Token.String()
	if token == "" {
		return false
	}

	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok && after == token {
			return true
		}
	}

	if c.matchedSubprotocol(r) != "" {
		return true
	}

	if c.config.AllowTokenQuery {
		if r.URL.Query().Get("token") == token {
			return true
		}
	}

	return false
}

func (c *VoiceChannel) matchedSubprotocol(r *http.Request) string {
	token := c.config.Token.String()
	for _, proto := range websocket.Subprotocols(r) {
		if after, ok := strings.CutPrefix(proto, "token."); ok && after == token {
			return proto
		}
	}
	return ""
}

func (c *VoiceChannel) readLoop(vc *voiceConn) {
	defer func() {
		vc.close()
		if removed := c.removeConnection(vc.id); removed != nil {
			logger.InfoCF("voice", "WebSocket client disconnected", map[string]any{
				"conn_id":    removed.id,
				"session_id": removed.sessionID,
			})
		}
	}()

	readTimeout := time.Duration(c.config.ReadTimeout) * time.Second
	if readTimeout <= 0 {
		readTimeout = 60 * time.Second
	}

	_ = vc.conn.SetReadDeadline(time.Now().Add(readTimeout))
	vc.conn.SetPongHandler(func(string) error {
		_ = vc.conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})

	pingInterval := time.Duration(c.config.PingInterval) * time.Second
	if pingInterval <= 0 {
		pingInterval = 30 * time.Second
	}
	go c.pingLoop(vc, pingInterval)

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_, raw, err := vc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(
				err,
				websocket.CloseGoingAway,
				websocket.CloseNormalClosure,
			) {
				logger.DebugCF("voice", "WebSocket read error", map[string]any{
					"conn_id": vc.id,
					"error":   err.Error(),
				})
			}
			return
		}

		_ = vc.conn.SetReadDeadline(time.Now().Add(readTimeout))

		var msg VoiceMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			errMsg := newError("invalid_message", "failed to parse message", nil)
			_ = vc.writeJSON(errMsg)
			continue
		}

		c.handleMessage(vc, msg)
	}
}

func (c *VoiceChannel) pingLoop(vc *voiceConn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if vc.closed.Load() {
				return
			}
			vc.writeMu.Lock()
			err := vc.conn.WriteMessage(websocket.PingMessage, nil)
			vc.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (c *VoiceChannel) handleMessage(vc *voiceConn, msg VoiceMessage) {
	switch msg.Type {
	case TypePing:
		pong := newMessage(TypePong, nil)
		pong.ID = msg.ID
		_ = vc.writeJSON(pong)

	case TypeMessageSend:
		c.handleMessageSend(vc, msg)

	default:
		errMsg := newError("unknown_type", fmt.Sprintf("unknown message type: %s", msg.Type), nil)
		_ = vc.writeJSON(errMsg)
	}
}

func (c *VoiceChannel) handleMessageSend(vc *voiceConn, msg VoiceMessage) {
	content, _ := msg.Payload[PayloadKeyContent].(string)
	if strings.TrimSpace(content) == "" {
		errMsg := newError("empty_content", "message content is empty", map[string]any{
			"request_id": msg.ID,
		})
		_ = vc.writeJSON(errMsg)
		return
	}

	sessionID := msg.SessionID
	if sessionID == "" {
		sessionID = vc.sessionID
	}

	chatID := "voice:" + sessionID
	senderID := "voice-user"

	metadata := map[string]string{
		"platform":   "voice",
		"session_id": sessionID,
		"conn_id":    vc.id,
	}

	logger.DebugCF("voice", "Received message", map[string]any{
		"session_id": sessionID,
		"preview":    truncate(content, 50),
	})

	sender := bus.SenderInfo{
		Platform:    "voice",
		PlatformID:  senderID,
		CanonicalID: identity.BuildCanonicalID("voice", senderID),
	}
	if !c.IsAllowedSender(sender) {
		return
	}

	inboundCtx := bus.InboundContext{
		Channel:   "voice",
		ChatID:    chatID,
		ChatType:  "direct",
		SenderID:  senderID,
		MessageID: msg.ID,
		Raw:       metadata,
	}

	c.HandleInboundContext(c.ctx, chatID, content, nil, inboundCtx, sender)
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
