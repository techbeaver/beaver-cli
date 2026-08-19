package client

import "context"

// HeaderCFConnectingIP names the caller's real address for the API's rate
// limiter and request log. The API honours it only from a peer inside its own
// trusted proxy list, so it is inert rather than dangerous when unconfigured.
const HeaderCFConnectingIP = "CF-Connecting-IP"

type callerIPKey struct{}

// WithCallerIP records the address a request is being made on behalf of. A
// hosted server sets this; a CLI running on the caller's own machine does not.
func WithCallerIP(ctx context.Context, ip string) context.Context {
	if ip == "" {
		return ctx
	}
	return context.WithValue(ctx, callerIPKey{}, ip)
}

// CallerIPFrom returns the address recorded by [WithCallerIP], or empty.
func CallerIPFrom(ctx context.Context) string {
	if v, ok := ctx.Value(callerIPKey{}).(string); ok {
		return v
	}
	return ""
}
