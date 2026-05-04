package mockserver

import "context"

// withDispatcherCtx returns a Background context for async webhook delivery.
// Broken out so tests can swap in a context with a short deadline if they
// want cancellation behavior.
func withDispatcherCtx() context.Context { return context.Background() }
