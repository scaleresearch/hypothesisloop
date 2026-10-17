package httpapi

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/danielgtaylor/huma/v2"
	"github.com/scaleresearch/hypothesisloop/controlplane/auth"
	"github.com/scaleresearch/hypothesisloop/controlplane/refusal"
)

// caller is the principal auth.Gate put in the request's context.
func caller(ctx context.Context, logger *zap.Logger) (auth.Principal, error) {
	principal, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Principal{}, auth.Internal(logger, "read the caller", errors.New("the request reached its handler with no principal"))
	}
	return principal, nil
}

// refused answers a refusal as it is and any other error as the platform failing op.
func refused(logger *zap.Logger, op string, err error) error {
	var r *refusal.Refusal
	if errors.As(err, &r) {
		return r
	}
	return auth.Internal(logger, op+" failed", err)
}

// rejected answers a refusal.Rejection as huma answers its own errors, and any other error as it
// is.
func rejected(err error) error {
	var r *refusal.Rejection
	if errors.As(err, &r) {
		return huma.NewError(r.Status, r.Error())
	}
	return err
}
