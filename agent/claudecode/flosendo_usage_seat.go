package claudecode

// Flosendo fork (EDEN-1789): the /usage probe runs on the asking channel's Claude seat. a.sessionEnv is agent-wide and
// holds the env of the last agent start, so a probe built from it reported the seat of whichever channel started an
// agent last. The engine's /usage puts the asking session's rule env in ctx (core.WithSessionEnv); without it (any
// other caller) the probe env is upstream's.

import (
	"context"

	"github.com/chenhg5/cc-connect/core"
)

func (a *Agent) usageProbeEnvFor(ctx context.Context) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seat, ok := core.SessionEnvFromContext(ctx); ok {
		saved := a.sessionEnv
		a.sessionEnv = seat
		defer func() { a.sessionEnv = saved }()
	}
	return a.runtimeEnvLocked()
}
