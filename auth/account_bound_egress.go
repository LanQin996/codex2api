package auth

import (
	"context"
	"strings"
)

type accountBoundEgressKey struct{}

// WithAccountBoundEgress freezes the caller-selected account proxy. Empty means
// explicit direct egress, not environment or global proxy inheritance.
func WithAccountBoundEgress(ctx context.Context, proxyURL string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, accountBoundEgressKey{}, strings.TrimSpace(proxyURL))
}

// AccountBoundEgress reports the frozen route and whether one was requested.
func AccountBoundEgress(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	value, ok := ctx.Value(accountBoundEgressKey{}).(string)
	return value, ok
}
