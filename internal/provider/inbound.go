package provider

import (
	"context"
	"errors"
)

// ErrNoCallerCredential means a pass-through provider was asked to forward a
// credential the caller never sent. It is the caller's mistake, not a fault, so
// handlers map it to 400 rather than 500.
var ErrNoCallerCredential = errors.New("no caller credential to forward")

// Inbound carries the few caller headers that are allowed to travel upstream.
// It is an allowlist by construction: nothing else from the client's request
// reaches a provider. The gateway sets it per request.
type Inbound struct {
	// Authorization is the caller's own upstream credential, forwarded by
	// oauth_passthrough providers. The gateway leaves it empty when the caller
	// authenticated with it, so a gateway API key never leaks upstream.
	Authorization string
	// AnthropicBeta is the client's beta feature list, forwarded to
	// anthropic-flavor upstreams.
	AnthropicBeta string
}

type inboundKey struct{}

// WithInbound attaches the forwardable caller headers to ctx. NewRequest reads
// them from there, so no signature on the routing path has to carry them.
func WithInbound(ctx context.Context, in Inbound) context.Context {
	return context.WithValue(ctx, inboundKey{}, in)
}

func inboundFrom(ctx context.Context) Inbound {
	in, _ := ctx.Value(inboundKey{}).(Inbound)
	return in
}

// HasCallerCredential reports whether the caller sent an upstream token of
// its own (one an oauth_passthrough provider could forward).
func HasCallerCredential(ctx context.Context) bool {
	return inboundFrom(ctx).Authorization != ""
}
