# Speculative turns (Pico Protocol) — draft

Status: **partially implemented**. Enables the voice bridge's preemptive
generation (library-claw `docs/software/INTERIM-FORWARDING.md`, Phase 2) to run a
turn *before* the user's final transcript is known, then keep or discard it
without polluting session history.

## Implementation status

- **Done — bridge side (library-claw), self-contained & unit-tested logic:**
  `ports.PicoChannel` gains `SendSpeculativeTurn` / `CommitTurn` / `AbortTurn`;
  `wsclient/manager.go` implements them (speculative `message.send` payload +
  `turn.commit`/`turn.abort` frames); `pico/fake` updated; `preemptive.go`
  coordinator now speculates with a `specID`, commits on match, aborts on
  divergence/timeout/failure/supersede; `preemptive_test.go` covers it. (Built
  blind — no Go toolchain in the authoring env; run `go test ./...` locally.)
- **Done (build-pending) — picoclaw, full path:** protocol constants
  (`turn.commit`/`turn.abort`, `speculative`/`speculation_id` payload keys);
  `bus.RawKey*`/`Control*` control keys; `handleMessageSend` threads the flag
  into `InboundContext.Raw`; `handleTurnControl` + `BaseChannel.EmitControl`
  publish commit/abort to the bus; agent-loop intercept
  (`agent.go` inbound loop) routes them to `handleSpeculationControl`;
  `speculationManager` (`speculation.go`) does begin/commit/abort
  (snapshot + truncate + restore summary, unit-tested in `speculation_test.go`);
  `turnState.speculative`/`speculationID` set in `pipeline_setup`; abort-on-tool
  -call in `pipeline_llm.go`. **Authored without a compiler** — expect to fix
  build errors; `TODO(build)` markers flag the spots I couldn't verify
  (terminal frame on tool-abort, exact dispatch field accessors).

> **Architecture correction:** an earlier draft had the *channel* snapshot and
> truncate session history directly. That is not possible — the pico channel is
> decoupled from the agent via the message bus and has **no session access**
> (`PicoChannel` holds no session store; `BaseChannel.bus` is unexported and
> pico is a subpackage). So commit/abort must reach the **agent** (the session
> owner). The tool-abort policy ("abort on any tool call") also requires the
> agent to know the turn is speculative. Both flow through the agent, below.

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
turn whose history write is **reversible** on an explicit commit/abort.

## Principle: snapshot at dispatch, truncate on abort

picoclaw already rolls back a turn's history writes — this design reuses that
pattern at the channel layer rather than inventing a parallel one.

The agent snapshots a restore point at turn start (`pipeline_setup.go:32`
`captureRestorePoint(history, summary)`; `turn_state.go:280-282` also records
`initialHistoryLength`) and rolls back via:

- `turn_state.go:694 restoreSession()` → `SetHistory(restorePoint) + SetSummary
  + Save` (called from `turn_coord.go:291`), and
- `steering.go:546-547` (hard abort / interruption) →
  `SetHistory(sessionKey, history[:initialHistoryLength])`.

That existing rollback is **turn-scoped**: the restore point lives on the
in-flight `turnState` and fires *during* a turn. Our commit/abort arrives
*after* the speculative reply has completed, when that `turnState` is gone — so
we cannot call `restoreSession` directly. Instead the **channel** takes the same
kind of snapshot itself, before dispatching the speculative turn, and reverts
with the same `SetHistory` one-liner on abort:

- A speculative `message.send` runs the **unchanged** agent pipeline against the
  **real** session. Before dispatching, the channel records
  `baseLen = len(GetHistory(realKey))` and the current summary.
- `turn.commit` → **no-op**: the turn already persisted to the real session, and
  by protocol it only commits when the final matched the provisional, so
  `[user=provisional, assistant=reply]` is correct as-is.
- `turn.abort` → `SetHistory(realKey, GetHistory(realKey)[:baseLen])` and restore
  the saved summary — the same operation `steering.go:547` already performs. The
  real session is back to exactly its pre-speculation state.

The reply streams back exactly as today, so the bridge captures/suppresses it.
The **match decision stays on the bridge** (it already holds both provisional and
final text); picoclaw only needs begin (speculative send) / commit / abort and
never compares transcripts. No agent-pipeline, LLM, or session-manager API
changes are required — only channel-layer bookkeeping over existing
`GetHistory`/`SetHistory`/`GetSummary`/`SetSummary` primitives.

### Why not a shadow session

An earlier draft forked a shadow session key and promoted its tail on commit.
That works but adds `Fork`/`PromoteTail`/`Remove` to the session manager and a
second session per speculation. Snapshot-and-truncate reuses the rollback pattern
the codebase already relies on, needs zero new session primitives, and keeps one
session per conversation. The trade-off: the speculative turn is *transiently*
present in the real session between dispatch and commit/abort. In voice that is
safe — turns are serialized and the bridge suppresses speaking until commit, so
nothing else reads the session in that window (see Correctness notes).

## Protocol additions

`pkg/channels/pico/protocol.go`:

```go
// Client → server
TypeTurnCommit = "turn.commit" // keep a speculative turn (no-op server-side today)
TypeTurnAbort  = "turn.abort"  // revert a speculative turn's history writes

// message.send payload (optional, additive — legacy clients omit them)
PayloadKeySpeculative   = "speculative"    // bool
PayloadKeySpeculationID = "speculation_id" // string, client-chosen, unique per session

// Server → client acks (recommended for backpressure / ordering)
TypeTurnCommitted = "turn.committed"
TypeTurnAborted   = "turn.aborted"
```

`turn.commit` / `turn.abort` carry `session_id` + `payload.speculation_id`.
Streamed `message.create` replies for a speculative turn SHOULD echo
`speculation_id` so the bridge can correlate (it can also rely on `id`/request_id
as today).

## Server-side changes (picoclaw) — agent-owned

Because the channel has no session access, commit/abort must be handled in the
agent, which owns `Sessions`. The truncation reuses the rollback the agent
already performs (`turn_state.go:694 restoreSession`, `steering.go:546-547`
`SetHistory(history[:initialHistoryLength])`); we just trigger it from an
external commit/abort instead of mid-turn steering.

### 1. Channel (DONE for the speculative send; remaining for commit/abort)

`pkg/channels/pico/pico.go`:

- `handleMessageSend`: **done** — parses `speculative` + `speculation_id` and
  threads them into `InboundContext.Raw` (`RawKeySpeculative`,
  `RawKeySpeculationID`). The turn then flows through the bus to the agent as
  normal, but tagged.
- dispatch switch (`handleMessage`): **remaining** — add
  `case TypeTurnCommit / TypeTurnAbort → handleTurnControl(pc, msg)`.
- `handleTurnControl`: **remaining** — the channel can't reach the bus directly
  (`BaseChannel.bus` is unexported; pico is a subpackage). Add an exported
  `BaseChannel.PublishControl(ctx, InboundMessage)` (or reuse
  `HandleInboundContext` with empty content) that publishes an `InboundMessage`
  with `Content:""`, `Raw[RawKeyControl] = "commit"|"abort"`, and
  `Raw[RawKeySpeculationID]`. Skip the typing/placeholder side effects in
  `HandleMessageWithContext` for control messages (guard on `RawKeyControl`).

### 2. Agent loop intercept

`pkg/agent/agent.go` inbound loop (`:166`, `msg := <-al.bus.InboundChan()`):
before `resolveSteeringTarget`/normal dispatch, check
`msg.Context.Raw[RawKeyControl]`. If set, call
`al.speculation.Handle(control, sessionKey, specID)` and `continue` — never run
a turn for a control message.

### 3. Speculation manager (new, `pkg/agent/speculation.go`)

Keyed by `speculation_id`. On a speculative turn start, record
`baseLen = len(Sessions.GetHistory(sessionKey))` + `baseSummary`. Handlers:

- `commit(specID)`: drop the entry. No history mutation — the turn already
  persisted, and the bridge only commits on a match, so the persisted
  `[user=provisional, assistant=reply]` is correct.
- `abort(specID)`: `h := Sessions.GetHistory(key)`; if `len(h) > baseLen` →
  `Sessions.SetHistory(key, h[:baseLen])`; `Sessions.SetSummary(key,
  baseSummary)`; `Sessions.Save(key)`; drop the entry. (Same op as
  `steering.go:547`.)

The baseLen snapshot is taken when the speculative turn is set up — wire it from
`pipeline_setup.go` (which already has `initialHistoryLength` /
`captureRestorePoint`) when the turnState carries the speculative flag.

No `SessionStore` / `SessionManager` additions: `GetHistory`, `SetHistory`,
`GetSummary`, `SetSummary`, `Save` already exist (`pkg/session/session_store.go`).

### 4. Speculative flag + abort-on-tool-call in the pipeline

- Thread the flag from `InboundContext.Raw[RawKeySpeculative]` into `turnState`
  (it already carries the InboundContext via `newTurnContext(...)` at
  `agent.go:564`); expose `ts.speculative`.
- In `pipeline_llm.go` where tool calls are detected
  (`len(exec.response.ToolCalls) > 0`, around `:566`): if `ts.speculative`,
  **abort** — roll back via `ts.restoreSession(agent)` (or the manager's abort),
  end the turn without executing any tool, and signal the bridge (terminal
  `message.create` with an `aborted_tool` marker) so it stops suppressing and
  falls back to a normal turn on the final. This is the chosen policy: a
  speculation never executes a tool, since side effects can't be rolled back.

### Single-flight + GC

- One speculation per session at a time. If a new speculative send arrives while
  one is pending (prefix grew), the channel aborts the prior (truncate) before
  starting the next. The bridge coordinator already cancels the prior in-flight
  speculation, so this is belt-and-suspenders.
- On connection drop or session end with a pending speculation, run the abort
  truncation (hook into `removeConnection` `:187` and session teardown), and
  TTL-sweep entries older than ~60 s so a lost commit/abort can't strand a
  half-applied turn in the real session.

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
  `SendSpeculativeTurn` instead of `SendUserTurn`, stores `specID` on the
  `speculation`.
- `take` on **match** → `CommitTurn(specID)` then return the captured result for
  the normal speak path (history correct, no second LLM call).
- `take` on **divergence** (or ctx cancel / superseding interim) →
  `AbortTurn(specID)`; caller falls back to a fresh, real `SendUserTurn(final)`.
- `closeSession` / barge-in → `AbortTurn` any in-flight spec.

This removes the "history caveat" that currently keeps Phase 2 off.

## Sequence

Match (the common case — edge final == last stable interim):

```
edge  interim "what books do you have" ─▶ bridge.onInterim
bridge ─ message.send{speculative,specID} ─▶ picoclaw: snapshot baseLen+summary, run turn on real session
picoclaw ─ message.create(stream…final) ─▶ bridge (OnStreamSpeak SUPPRESSED, reply captured)
edge  final "what books do you have" ─▶ bridge.take: match
bridge ─ turn.commit{specID} ─▶ picoclaw: drop pending entry (history already correct)
bridge speaks the captured reply.   # claw latency was paid during STT
```

Divergence:

```
… speculative run persisted [user=provisional, assistant=reply] to real session …
edge  final "what wines do you stock" ─▶ bridge.take: mismatch
bridge ─ turn.abort{specID} ─▶ picoclaw: SetHistory(real, h[:baseLen]) + restore summary
bridge ─ message.send "what wines do you stock" (normal turn, clean history)
```

## Correctness notes / edge cases

- **Transient visibility**: between dispatch and commit/abort the speculative
  turn is present in the real session. Voice turns are serialized and the bridge
  suppresses speaking until commit, so no other reader observes it. A non-voice
  client multiplexing the same session concurrently would — so speculation is a
  voice-channel feature; gate it off for shared/multiplexed sessions.
- **Tool calls** persist to the real session too and are truncated by the abort
  along with the user/assistant messages. Tool **side effects** are NOT rolled
  back. Bound speculation to turns claw answers without tools, or **abort the
  moment the shadow turn requests a non-readonly tool** (open decision — pick one
  before implementing; it shapes the server guard).
- **Summary mutation**: if the speculative turn summarizes, abort restores the
  snapshotted `baseSummary`; commit keeps the new one. Snapshot the summary at
  dispatch (above).
- **Ordering vs undelivered system messages**: with reversible writes the bridge
  can still inject `voice.reply.undelivered` (and other system context) on the
  real session *before* `turn.commit`, preserving today's ordering — on a match
  the speculative user/assistant pair stays, the system note slots ahead of the
  next turn as usual.
- **Concurrent real growth**: serialized voice turns mean `len(real)` at abort ==
  `baseLen + speculative tail`, so `h[:baseLen]` is exact. If a future
  multiplexed writer could grow the session mid-speculation, switch that path to
  the persisted-tail match (`matchingTurnMessageTail`, `turn_state.go:670`) used
  by `refreshRestorePointFromSession` rather than a raw length cut.
- **Lost commit/abort** (crash): TTL sweep aborts orphaned speculations so a
  half-applied turn can't linger.

## Testing plan

- `pkg/channels/pico`: speculative send persists to the real session and runs the
  pipeline; commit leaves it; abort truncates back to `baseLen` and restores
  summary; duplicate specID rejected; connection drop aborts pending. (`pico_test.go`
  already has a harness.)
- `pkg/session`: assert `SetHistory(h[:baseLen])` + `SetSummary(base)` round-trips
  a snapshot (the abort primitive) — mirrors the `steering.go:547` rollback.
- Bridge: coordinator commit-on-match / abort-on-divergence (extend
  `preemptive_test.go` with a fake `PicoChannel` recording commit/abort calls).
- Integration: end-to-end with `STT_FORWARD_INTERIM=1` firmware, asserting a
  matched turn issues exactly one LLM call and a diverged turn leaves no phantom
  history.
