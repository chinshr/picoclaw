# Speculative turns (Pico Protocol) — draft

Status: **draft / proposal**. Enables the voice bridge's preemptive generation
(library-claw `docs/software/INTERIM-FORWARDING.md`, Phase 2) to run a turn
*before* the user's final transcript is known, then keep or discard it without
polluting session history.

## Problem

The voice bridge wants to start a turn on a provisional transcript (an STT
interim) while the rest of transcription finishes, so claw latency overlaps STT
instead of stacking after it. Today the only primitive is `message.send`, which
runs the turn **and commits** the user message (and the assistant reply) to the
session history at `pipeline_setup.go:79-81` and `pipeline_finalize.go:49`.

So a speculation that turns out wrong (final ≠ provisional) leaves a phantom user
turn in history, and even a correct one runs *before* any
`voice.reply.undelivered` system message the bridge would normally inject first.
The bridge therefore keeps preemptive generation OFF until picoclaw can run a
turn whose history write is **deferred** until an explicit commit.

## Principle: run on a shadow session; commit promotes, abort drops

A speculative `message.send` runs the **unchanged** agent pipeline against a
**shadow session key** forked (copy-on-read) from the real session. The reply
streams back exactly as today, so the bridge can capture/suppress it. The real
session is untouched until:

- `turn.commit` → promote the shadow's appended tail (user + assistant + any
  tool messages) onto the real session, then drop the shadow.
- `turn.abort` → drop the shadow. The real session never saw the turn.

The **match decision stays on the bridge** (it already holds both provisional
and final text). picoclaw only needs begin (speculative send) / commit / abort —
it never compares transcripts. This keeps the agent pipeline and the LLM path
completely unchanged; all new logic is in the channel + session layers.

## Protocol additions

`pkg/channels/pico/protocol.go`:

```go
// Client → server
TypeTurnCommit = "turn.commit" // promote a speculative turn into real history
TypeTurnAbort  = "turn.abort"  // discard a speculative turn

// message.send payload (optional, additive — legacy clients omit them)
PayloadKeySpeculative   = "speculative"    // bool
PayloadKeySpeculationID = "speculation_id" // string, client-chosen, unique per session

// Server → client acks (optional but recommended for backpressure)
TypeTurnCommitted = "turn.committed"
TypeTurnAborted   = "turn.aborted"
```

`turn.commit` / `turn.abort` frames carry `session_id` + `payload.speculation_id`.
Streamed `message.create` replies for a speculative turn SHOULD echo
`speculation_id` in their payload so the bridge can correlate (it can also rely
on `id`/request_id as today).

## Server-side changes (picoclaw)

### 1. Shadow-key helper

`shadowKey(realKey, specID) = realKey + "#spec:" + specID`. The `#` segment keeps
it outside any real session-key namespace.

### 2. Session manager: fork / promote / drop

`pkg/session/manager.go` (+ the `SessionStore` interface in
`pkg/session/session_store.go`, implemented by both backends):

```go
// Fork seeds child with a deep copy of parent's current history (+ summary).
// No-op base if parent is unknown (child starts empty).
func (sm *SessionManager) Fork(parentKey, childKey string)

// PromoteTail appends child.history[baseLen:] onto parent in order, then
// removes child. baseLen is the parent length captured at Fork time, returned
// by Fork so the caller can detect concurrent real-session growth.
func (sm *SessionManager) PromoteTail(childKey, parentKey string, baseLen int)

// Remove deletes a session key outright (used by abort + after promote).
func (sm *SessionManager) Remove(key string)
```

`AddFullMessage` already auto-creates a session, and `GetHistory` deep-copies, so
`Fork` is `base := GetHistory(parent); for m := range base { AddFullMessage(child, m) }`
plus summary copy; `Remove` deletes `sm.sessions[key]` under the lock. Keep them
as first-class methods (not loops at the call site) so history-filter semantics
stay centralized.

### 3. Channel: speculative routing + commit/abort handlers

`pkg/channels/pico/pico.go`:

- `handleMessageSend` (`:1108`): parse `speculative` + `speculation_id`. When
  speculative, fork the shadow, record a pending entry
  `{specID → (realKey, shadowKey, baseLen, conn)}` under a new mutex, and route
  `HandleInboundContext` with the **shadow** chatID/sessionKey. Everything
  downstream (pipeline, streaming reply) is unchanged — it just writes to the
  shadow. Reject a second speculative send with the same in-flight `specID`.

- `handleMessageSend` dispatch (`:1095`): add
  `case TypeTurnCommit: c.handleTurnCommit(pc, msg)` and
  `case TypeTurnAbort: c.handleTurnAbort(pc, msg)`.

- `handleTurnCommit(specID)`: look up the pending entry; `PromoteTail(shadow,
  real, baseLen)`; clear the entry; ack `turn.committed`. (By protocol the bridge
  only commits **after** it has received the speculative reply's final
  `message.create`, so the shadow turn is already complete — no wait needed. If
  defensiveness is wanted, gate on a per-spec "reply finalized" flag set when the
  terminal `message.create` is emitted for that shadow session.)

- `handleTurnAbort(specID)`: `Remove(shadow)`; clear the entry; ack
  `turn.aborted`. Best-effort cancel of an in-flight shadow run via its context
  if the agent is still generating.

### 4. Orphan GC

If a connection drops or a session ends with pending speculations, `Remove`
every shadow for that session (hook into `removeConnection` `:187` and session
teardown). Also TTL-sweep shadows older than, say, 60 s so a lost commit/abort
can't leak memory.

## Bridge-side changes (library-claw)

`bridge/go/internal/pico/wsclient/manager.go` — the `message.send` builder at
`:327` gains the two payload fields, plus commit/abort senders:

```go
func (m *Manager) SendSpeculativeTurn(ctx, sid, content, specID string, timeoutSec float64) ports.PicoTurnResult
func (m *Manager) CommitTurn(ctx, sid, specID string) error // sends turn.commit
func (m *Manager) AbortTurn(ctx, sid, specID string) error  // sends turn.abort
```

`SendSpeculativeTurn` is `SendUserTurn` with
`payload{speculative:true, speculation_id:specID}` — same streaming/await/timeout
path, so the streamed reply still flows through `OnStreamSpeak` (which the
coordinator suppresses).

`bridge/go/internal/ports/ports.go` — extend `PicoChannel`:

```go
SendSpeculativeTurn(ctx context.Context, sessionID, content, speculationID string, requestTimeoutSec float64) PicoTurnResult
CommitTurn(ctx context.Context, sessionID, speculationID string) error
AbortTurn(ctx context.Context, sessionID, speculationID string) error
```

`preemptive.go` (the coordinator already built in Phase 2) changes:

- `onInterim` generates a `specID` (e.g. `spec_` + hex), calls
  `SendSpeculativeTurn` instead of `SendUserTurn`, and stores `specID` on the
  `speculation`.
- `take` on **match** → `CommitTurn(specID)` then return the captured result for
  the normal speak path (history now correct, no second LLM call).
- `take` on **divergence** (or ctx cancel / new interim superseding) →
  `AbortTurn(specID)`; caller falls back to a fresh, real `SendUserTurn(final)`.
- `closeSession` / barge-in → `AbortTurn` any in-flight spec.

This removes the "history caveat" that currently keeps Phase 2 off.

## Sequence

Match (the common case — edge final == last stable interim):

```
edge  interim "what books do you have" ─▶ bridge.onInterim
bridge ─ message.send{speculative,specID} ─▶ picoclaw: Fork(real→shadow), run on shadow
picoclaw ─ message.create(stream…final) ─▶ bridge (OnStreamSpeak SUPPRESSED, reply captured)
edge  final "what books do you have" ─▶ bridge.take: match
bridge ─ turn.commit{specID} ─▶ picoclaw: PromoteTail(shadow→real), drop shadow
bridge speaks the captured reply.   # claw latency was paid during STT
```

Divergence:

```
… speculative run on shadow as above …
edge  final "what wines do you stock" ─▶ bridge.take: mismatch
bridge ─ turn.abort{specID} ─▶ picoclaw: drop shadow (real history untouched)
bridge ─ message.send "what wines do you stock" (normal turn)
```

## Correctness notes / edge cases

- **Tool calls in a speculation** write to the shadow and are promoted as part of
  the tail on commit — history stays internally consistent. Side effects of tools
  themselves are NOT rolled back; speculative turns should run with tool-use
  policy that avoids irreversible side effects, or speculation should be limited
  to turns the model answers without tools. Call this out as a config bound
  (e.g. abort if the shadow turn requests a non-readonly tool).
- **Summary mutation**: if the shadow turn triggers summarization, promote the
  shadow summary on commit; drop it on abort. `Fork` copies the summary so the
  shadow starts consistent.
- **Ordering vs undelivered system messages**: with commit/abort the real-session
  write happens at commit time, so the bridge can still inject
  `voice.reply.undelivered` (and any other system context) on the real session
  *before* `turn.commit`, preserving today's ordering.
- **Concurrent real growth**: in voice, turns are serialized, so `baseLen` equals
  the real length at commit. `PromoteTail` appends the tail regardless; if some
  other writer grew the real session meanwhile, the tail still appends in order
  (acceptable) — flag if a non-voice multiplexed client ever shares the session.
- **Multiple speculations per turn** (prefix grows): each gets its own `specID`
  and shadow; superseded ones are aborted when the next starts (the coordinator
  already cancels the prior in-flight speculation).
- **Lost commit/abort** (crash): TTL sweep drops orphaned shadows.

## Testing plan

- `pkg/session`: Fork copies base + summary; PromoteTail appends tail and removes
  child; Remove deletes; orphan TTL sweep.
- `pkg/channels/pico`: speculative send routes to shadow and leaves real history
  empty; commit promotes; abort leaves real empty; duplicate specID rejected;
  connection drop GCs shadows. (`pico_test.go` already has a harness.)
- Bridge: coordinator commit-on-match / abort-on-divergence (extend
  `preemptive_test.go` with a fake `PicoChannel` recording commit/abort calls).
- Integration: end-to-end with `STT_FORWARD_INTERIM=1` firmware, asserting a
  matched turn issues exactly one LLM call and a diverged turn leaves no phantom
  history.
```
