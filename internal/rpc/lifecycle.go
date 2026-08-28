package rpc

// lifecycleState is the LSP server lifecycle state machine. It gates which
// requests and notifications are accepted, per the LSP base protocol:
// nothing but "initialize" is served before initialization, and nothing but
// "exit" is served after shutdown.
type lifecycleState int32

const (
	stateUninitialized lifecycleState = iota
	stateInitialized
	stateShuttingDown
	stateExited
)
