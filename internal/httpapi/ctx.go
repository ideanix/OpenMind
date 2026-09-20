package httpapi

import (
	"context"

	"github.com/ideanix/openmind/internal/auth"
)

func contextWithIdentity(ctx context.Context, id auth.Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

type ctxT = context.Context
