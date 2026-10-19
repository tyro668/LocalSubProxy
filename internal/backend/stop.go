package backend

import (
	"context"
	"regexp"
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

// StopReason canonicalises a CLI stop reason: "EndTurn", "end_turn",
// "end-turn" and "END_TURN" all become "end_turn". Unknown reasons come
// back squashed to lower case.
func StopReason(s string) string {
	squashed := strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(s)))
	switch squashed {
	case "endturn", "stop", "completed", "success":
		return "end_turn"
	case "maxtokens", "length":
		return "max_tokens"
	case "maxturnrequests", "maxturns":
		return "max_turn_requests"
	case "cancelled", "canceled", "interrupted":
		return "cancelled"
	case "refusal", "refused":
		return "refusal"
	}
	return squashed
}

// Finish maps a CLI stop reason to the LMBP finish, or the error it means.
func Finish(vendor, stop string, usage *lmbp.Usage) (*lmbp.TaskDone, error) {
	switch StopReason(stop) {
	case "end_turn":
		return &lmbp.TaskDone{FinishReason: "stop", Usage: usage}, nil
	case "max_tokens":
		return &lmbp.TaskDone{FinishReason: "length", Usage: usage}, nil
	case "cancelled":
		return nil, context.Canceled
	}
	return nil, Failure("UPSTREAM_ERROR: " + vendor + " stopped with " + stop)
}

var (
	rateLimited = regexp.MustCompile(`(?i)rate[ _-]?limit|too many requests|\b429\b`)
	quota       = regexp.MustCompile(`(?i)quota|usage[ _-]?limit|resource[ _-]?exhausted|insufficient[ _-]?(credit|balance|fund)|out of credits|weekly (pool|limit)|limit (reached|exceeded)|exceeded your`)
	auth        = regexp.MustCompile(`(?i)not authenticated|unauthenticated|unauthorized|authentication required|\b401\b|please (log|sign) ?in|login required`)
)

// CLIFailure classifies an error a vendor CLI reported, so rate limits and
// exhausted quotas stay retryable (PROVIDER_RATE_LIMITED, QUOTA_EXHAUSTED).
func CLIFailure(vendor, message string) error {
	code := "UPSTREAM_ERROR"
	switch {
	case rateLimited.MatchString(message):
		code = "PROVIDER_RATE_LIMITED"
	case quota.MatchString(message):
		code = "QUOTA_EXHAUSTED"
	case auth.MatchString(message):
		code = "AUTH_REQUIRED"
	}
	return Failure(code + ": " + vendor + ": " + message)
}
