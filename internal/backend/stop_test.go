package backend

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tyro668/avora/internal/lmbp"
)

func TestStopReasonVariants(t *testing.T) {
	for in, want := range map[string]string{"EndTurn": "end_turn", "end_turn": "end_turn", "END-TURN": "end_turn", "MaxTokens": "max_tokens",
		"max_tokens": "max_tokens", "MaxTurnRequests": "max_turn_requests", "Cancelled": "cancelled", "canceled": "cancelled", "Refusal": "refusal", "tool_use": "tooluse"} {
		if got := StopReason(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	if d, err := Finish("X", "EndTurn", nil); err != nil || d.FinishReason != "stop" {
		t.Fatal(d, err)
	}
	if d, err := Finish("X", "MaxTokens", nil); err != nil || d.FinishReason != "length" {
		t.Fatal(d, err)
	}
	if _, err := Finish("X", "Cancelled", nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Finish("X", "Refusal", nil); ErrorBody(err).Code != lmbp.ErrUpstream {
		t.Fatal(err)
	}
}

func TestCLIFailureClassification(t *testing.T) {
	for msg, want := range map[string]string{
		"Rate limit exceeded, retry in 20s":            "PROVIDER_RATE_LIMITED",
		"HTTP 429 Too Many Requests":                   "PROVIDER_RATE_LIMITED",
		"You have exhausted your weekly pool":          "QUOTA_EXHAUSTED",
		"RESOURCE_EXHAUSTED: quota exceeded for model": "QUOTA_EXHAUSTED",
		"Usage limit reached for your plan":            "QUOTA_EXHAUSTED",
		"Not authenticated, run grok login":            "AUTH_REQUIRED",
		"model does-not-exist is not recognized":       "UPSTREAM_ERROR",
	} {
		b := ErrorBody(CLIFailure("Grok", msg))
		retry := want == "PROVIDER_RATE_LIMITED" || want == "QUOTA_EXHAUSTED"
		if b.Code != lmbp.ErrUpstream || !strings.HasPrefix(b.Message, want+":") || b.Retryable != retry {
			t.Errorf("%q -> %+v, want %s", msg, b, want)
		}
	}
}
