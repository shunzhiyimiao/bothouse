package core

// ensureSubTaskTableForTest calls ensureSubTaskTable the way these tests meant before it took a
// session key and showed the table only in workspace channels (upstream, 2026-10-26): the tests'
// channel counts as a workspace. Fork-local fix so ./core compiles; drop once upstream fixes the
// call sites itself.
func ensureSubTaskTableForTest(e *Engine, state *interactiveState) {
	if e.workspaceChatForTest == nil {
		e.workspaceChatForTest = func(string) bool { return true }
	}
	e.ensureSubTaskTable(state, "test:workspace")
}
