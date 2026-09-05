package remotetest

import "github.com/srhg-ai-7cef3f93/ben/internal/remote"

// The fake satisfies every seam of the v2 boundary, checked at compile time
// rather than at the first test that happens to use one.
//
// It matters more here than for an ordinary fake. A fake that has drifted out of
// an interface fails loudly when a test wires it; a fake that satisfies the
// interface and models the wrong thing does not, which is why the *behavioural*
// halves of these contracts — a disconnect that is not a cancel, a signal that is
// not a termination — are asserted in internal/remote's own tests rather than
// left to this declaration.
var (
	_ remote.WorkspaceBackend = (*Workspaces)(nil)
	_ remote.ProcessBackend   = (*Backend)(nil)
	_ remote.HookExec         = (*Backend)(nil)
	_ remote.DurableConsumer  = (*Consumer)(nil)
	_ remote.HookStore        = (*MemHookStore)(nil)
	_ remote.Store            = (*MemStore)(nil)
)
