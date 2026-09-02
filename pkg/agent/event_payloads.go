package agent

import "time"

// TurnEndStatus describes the terminal state of a turn.
type TurnEndStatus string

const (
	// TurnEndStatusCompleted indicates the turn finished normally.
	TurnEndStatusCompleted TurnEndStatus = "completed"
	// TurnEndStatusError indicates the turn ended because of an error.
	TurnEndStatusError TurnEndStatus = "error"
	// TurnEndStatusAborted indicates the turn was hard-aborted and rolled back.
	TurnEndStatusAborted TurnEndStatus = "aborted"
)

// TurnStartPayload describes the start of a turn.
type TurnStartPayload struct {
	UserMessage string
	MediaCount  int
}

const (
	skillContextTriggerInitialBuild        = "initial_build"
	skillContextTriggerContextRetryRebuild = "context_retry_rebuild"
)

type SkillContextSnapshot struct {
	Sequence   int      `json:"sequence"`
	Trigger    string   `json:"trigger"`
	SkillNames []string `json:"skill_names,omitempty"`
}

type ToolExecutionRecord struct {
	Name         string   `json:"name"`
	Success      bool     `json:"success"`
	ErrorSummary string   `json:"error_summary,omitempty"`
	SkillNames   []string `json:"skill_names,omitempty"`
}

// TurnEndPayload describes the completion of a turn.
type TurnEndPayload struct {
	Status          TurnEndStatus
	Workspace       string
	Iterations      int
	Duration        time.Duration
	FinalContentLen int

	// Provider accounting (finding 15). Duration minus ProviderDuration is the
	// time the turn spent OUTSIDE the model — tools, compaction, steering,
	// waiting — which is the half this repo's findings can actually change.
	// ProviderCalls counts real calls only: a speculation carry-forward replay
	// (finding 01) made no call and is excluded, so it cannot inflate the
	// total with time nobody spent. ProviderTTFT sums only the calls that
	// streamed, so read it against ProviderCalls, never as a per-call mean.
	// MaxPromptChars is the largest single request in the turn.
	ProviderCalls    int
	ProviderDuration time.Duration
	ProviderTTFT     time.Duration
	MaxPromptChars   int

	UserMessage           string
	FinalContent          string
	ActiveSkills          []string
	AttemptedSkills       []string
	FinalSuccessfulPath   []string
	SkillContextSnapshots []SkillContextSnapshot
	ToolKinds             []string
	ToolExecutions        []ToolExecutionRecord
}

// LLMRequestPayload describes an outbound LLM request.
type LLMRequestPayload struct {
	Model         string
	MessagesCount int
	ToolsCount    int
	MaxTokens     int
	Temperature   float64
	// PromptChars is the assembled request size: every message's content plus
	// the serialized tool definitions. Counted here rather than taken from the
	// provider's usage because usage only exists once the call RETURNS — and a
	// call that never returns is exactly the case worth measuring. Chars, not
	// tokens: the tokenizer is the provider's, and a ratio is enough to tell a
	// 4k prompt from a 60k one.
	PromptChars int
	ToolsChars  int
}

// LLMResponsePayload describes an inbound LLM response.
type LLMResponsePayload struct {
	ContentLen   int
	ToolCalls    int
	HasReasoning bool
	// Replayed is true when this response was NOT produced by a provider call
	// this turn: it is an aborted speculation's tool-call decision being
	// replayed into the re-run (speculationManager carry-forward). Duration is
	// ~0 for these, so without this flag a replay is indistinguishable in the
	// event stream from an impossibly fast model.
	Replayed bool
	// Duration is the wall time of this single LLM call: from just after
	// the request is assembled to the response being in hand, including
	// streaming, retries and provider fallback. A turn that runs several
	// tool rounds makes one call per round, and only the sum was
	// previously observable (agent.turn.end duration_ms).
	Duration time.Duration

	// TTFT is time to first token: from the same start as Duration to the
	// first streamed chunk. Zero when the call did not stream (no streamer for
	// the channel, a non-streaming provider, or a replay), which is NOT the
	// same as "instant" — read it together with Streamed.
	//
	// This is the split finding 15 exists for. Duration alone cannot tell a
	// provider that took 14 s to say the first word from one that streamed
	// promptly and generated for 14 s. The first is queueing and prefill and
	// scales with PromptChars; the second scales with CompletionTokens. They
	// have nothing in common and no shared fix.
	TTFT     time.Duration
	Streamed bool
	Chunks   int

	// Usage as the provider reported it. Zero when the provider omitted it;
	// PromptChars on the request event is the always-available fallback.
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// LLMDeltaPayload describes a streamed LLM delta.
type LLMDeltaPayload struct {
	ContentDeltaLen   int
	ReasoningDeltaLen int
}

// LLMRetryPayload describes a retry of an LLM request.
type LLMRetryPayload struct {
	Attempt    int
	MaxRetries int
	Reason     string
	Error      string
	Backoff    time.Duration
}

// ContextCompressReason identifies why emergency compression ran.
type ContextCompressReason string

const (
	// ContextCompressReasonProactive indicates compression before the first LLM call.
	ContextCompressReasonProactive ContextCompressReason = "proactive_budget"
	// ContextCompressReasonRetry indicates compression during context-error retry handling.
	ContextCompressReasonRetry ContextCompressReason = "llm_retry"
	// ContextCompressReasonSummarize indicates post-turn async summarization.
	ContextCompressReasonSummarize ContextCompressReason = "summarize"
)

// ContextCompressPayload describes a forced history compression.
type ContextCompressPayload struct {
	Reason            ContextCompressReason
	DroppedMessages   int
	RemainingMessages int
}

// SessionSummarizePayload describes a completed async session summarization.
type SessionSummarizePayload struct {
	SummarizedMessages int
	KeptMessages       int
	SummaryLen         int
	OmittedOversized   bool
}

// ToolExecStartPayload describes a tool execution request.
type ToolExecStartPayload struct {
	Tool      string
	Arguments map[string]any
}

// ToolExecEndPayload describes the outcome of a tool execution.
type ToolExecEndPayload struct {
	Tool       string
	Duration   time.Duration
	ForLLMLen  int
	ForUserLen int
	IsError    bool
	Async      bool
}

// ToolExecSkippedPayload describes a skipped tool call.
type ToolExecSkippedPayload struct {
	Tool   string
	Reason string
}

// SteeringInjectedPayload describes steering messages appended before the next LLM call.
type SteeringInjectedPayload struct {
	Count           int
	TotalContentLen int
}

// FollowUpQueuedPayload describes an async follow-up queued back into the inbound bus.
type FollowUpQueuedPayload struct {
	SourceTool string
	ContentLen int
}

type InterruptKind string

const (
	InterruptKindSteering InterruptKind = "steering"
	InterruptKindGraceful InterruptKind = "graceful"
	InterruptKindHard     InterruptKind = "hard_abort"
)

// InterruptReceivedPayload describes accepted turn-control input.
type InterruptReceivedPayload struct {
	Kind       InterruptKind
	Role       string
	ContentLen int
	QueueDepth int
	HintLen    int
}

// SubTurnSpawnPayload describes the creation of a child turn.
type SubTurnSpawnPayload struct {
	AgentID      string
	Label        string
	ParentTurnID string
}

// SubTurnEndPayload describes the completion of a child turn.
type SubTurnEndPayload struct {
	AgentID string
	Status  string
}

// SubTurnResultDeliveredPayload describes delivery of a sub-turn result.
type SubTurnResultDeliveredPayload struct {
	TargetChannel string
	TargetChatID  string
	ContentLen    int
}

// SubTurnOrphanPayload describes a sub-turn result that could not be delivered.
type SubTurnOrphanPayload struct {
	ParentTurnID string
	ChildTurnID  string
	Reason       string
}

// ErrorPayload describes an execution error inside the agent loop.
type ErrorPayload struct {
	Stage   string
	Message string
}
