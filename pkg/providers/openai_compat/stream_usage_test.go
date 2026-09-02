package openai_compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A streamed OpenAI-compatible response carries NO usage unless the request
// asks for it. Without usage there is no prompt_tokens_details.cached_tokens,
// and Kimi/Moonshot prefix caching is fully automatic and invisible in the
// request — so that one response field is the entire evidence of whether a
// 39 KB system prompt is being served from cache or prefilled cold on every
// call. picoclaw asked for a stream and never asked for usage, so the number
// that decides library-claw's voice-turn-triage finding 15 simply never
// existed.
func TestStreamRequestsUsage(t *testing.T) {
	var body map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	p := NewProvider("key", server.URL, "")
	if _, err := p.ChatStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, "kimi-k3", nil,
		func(string) {},
	); err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}

	opts, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing from request body: %#v", body)
	}
	if include, _ := opts["include_usage"].(bool); !include {
		t.Fatalf("include_usage = %v, want true", opts["include_usage"])
	}
}

// Not every OpenAI-compatible endpoint accepts the field, and an unknown field
// is a 422 on several of them. The opt-out keeps this from becoming the next
// "works on OpenAI, breaks everywhere else" default.
func TestStreamUsageCanBeDisabledPerModel(t *testing.T) {
	var body map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	p := NewProvider("key", server.URL, "")
	if _, err := p.ChatStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, "some-model",
		map[string]any{"stream_include_usage": false},
		func(string) {},
	); err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}

	if _, present := body["stream_options"]; present {
		t.Fatalf("stream_options sent despite opt-out: %#v", body)
	}
}

// The usage frame arrives as a final chunk with an EMPTY choices array. A
// parser that assumes every frame has a choice would drop it — which would
// reintroduce the exact blind spot this change exists to remove.
func TestUsageFrameWithNoChoicesIsCaptured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12000,"+
			"\"completion_tokens\":6,\"total_tokens\":12006,"+
			"\"prompt_tokens_details\":{\"cached_tokens\":11776}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	p := NewProvider("key", server.URL, "")
	resp, err := p.ChatStream(
		t.Context(),
		[]Message{{Role: "user", Content: "hi"}},
		nil, "kimi-k3", nil,
		func(string) {},
	)
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("usage was dropped; cached_tokens is unobservable")
	}
	if resp.Usage.PromptTokens != 12000 {
		t.Fatalf("prompt_tokens = %d, want 12000", resp.Usage.PromptTokens)
	}
	if got := resp.Usage.Cached(); got != 11776 {
		t.Fatalf("cached tokens = %d, want 11776", got)
	}
}
