package backend

import (
	"context"
	"errors"
	"strings"

	"github.com/tyro668/avora/internal/lmbp"
)

// Failure is a classified backend error; its prefix (POLICY_DENIED, QUOTA_EXHAUSTED, ...) picks the LMBP code.
type Failure string

func (e Failure) Error() string { return string(e) }
func ErrorBody(err error) *lmbp.ErrorBody {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return &lmbp.ErrorBody{Code: lmbp.ErrCancelled, Message: "cancelled"}
	case errors.Is(err, context.DeadlineExceeded):
		return &lmbp.ErrorBody{Code: lmbp.ErrTimeout, Message: "runtime deadline elapsed"}
	}
	var known Failure
	if errors.As(err, &known) {
		switch {
		case strings.HasPrefix(err.Error(), "POLICY_DENIED"):
			return &lmbp.ErrorBody{Code: lmbp.ErrForbidden, Message: err.Error()}
		case strings.HasPrefix(err.Error(), "CAPABILITY_UNSUPPORTED"):
			return &lmbp.ErrorBody{Code: lmbp.ErrUnsupported, Message: err.Error()}
		}
	}
	retry := errors.As(err, &known) && (strings.HasPrefix(err.Error(), "PROVIDER_RATE_LIMITED") || strings.HasPrefix(err.Error(), "QUOTA_EXHAUSTED"))
	return &lmbp.ErrorBody{Code: lmbp.ErrUpstream, Message: err.Error(), Retryable: retry}
}
