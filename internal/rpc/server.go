package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.lsp.dev/protocol"
)

const (
	methodNotFoundCode       = int32(protocol.ErrorCodesMethodNotFound)
	serverNotInitializedCode = int32(protocol.ErrorCodesServerNotInitialized)
	invalidRequestCode       = int32(protocol.ErrorCodesInvalidRequest)
	internalErrorCode        = int32(protocol.ErrorCodesInternalError)
	requestCancelledCode     = int32(protocol.LSPErrorCodesRequestCancelled)
)

// RequestHandler answers a JSON-RPC request. Returning a non-nil error other
// than *Error is reported to the client as InternalError.
type RequestHandler func(ctx context.Context, params json.RawMessage) (result any, err error)

// NotificationHandler handles a JSON-RPC notification. Notifications have no
// response channel; a returned error is only logged.
type NotificationHandler func(ctx context.Context, params json.RawMessage) error

type requestReg struct {
	priority Priority
	handler  RequestHandler
}

// Server dispatches JSON-RPC 2.0 requests and notifications received over
// stdio to registered handlers, enforcing the LSP initialize/shutdown/exit
// lifecycle and $/cancelRequest. Handlers are registered with Handle and
// HandleNotification before Serve is called; Server itself carries no LSP
// domain knowledge beyond the lifecycle method names and error codes.
type Server struct {
	logger *log.Logger

	requestHandlers      map[string]requestReg
	notificationHandlers map[string]NotificationHandler

	pools map[Priority]*pool

	cancels cancelRegistry

	state   atomic.Int32
	exitErr *ExitError

	queuesMu sync.Mutex
	queues   map[string]*notifQueue

	pendingMu sync.Mutex
	pending   map[string]chan *message // keyed by the request id Request assigned, awaiting the client's response
	nextID    atomic.Int64

	// wg tracks in-flight request and notification handler goroutines, plus
	// every Go-launched detached background goroutine, so Serve can wait for
	// their responses/side effects to complete before returning, instead of
	// racing the write of a still-running handler against the caller
	// closing the connection.
	wg sync.WaitGroup

	// ctx is the session-lifetime context: a child of the ctx passed to
	// Serve, canceled when Serve returns for any reason. An atomic.Pointer,
	// not a plain field, so Context/Go are safe to call from any goroutine
	// — not just ones a handler dispatched from Serve's own loop, which
	// would otherwise be the only calls guaranteed a happens-before edge
	// against Serve's write. Nil before Serve is called.
	ctx atomic.Pointer[context.Context]

	// drainTimeout bounds how long Serve's shutdown-time drain (see drain)
	// waits for in-flight handlers and Go-launched background work before
	// abandoning them, set by NewServer to defaultDrainTimeout.
	drainTimeout time.Duration

	// requestTimeout bounds how long a single Background-priority request
	// (see dispatchRequest) may run before its context is canceled with
	// context.DeadlineExceeded; Interactive requests are never bounded by
	// it. Set once by NewServer from GOLANCE_REQUEST_TIMEOUT (or
	// defaultRequestTimeout); <= 0 disables the bound entirely.
	requestTimeout time.Duration

	conn *conn
}

// Option configures a Server built by NewServer.
type Option func(*Server)

// WithLogger sets the logger used to report notification handler errors.
// The default is log.Default().
func WithLogger(l *log.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// WithBackgroundWorkers bounds how many Background-priority requests run
// concurrently. n <= 0 means unbounded. The default is defaultBackgroundWorkers().
func WithBackgroundWorkers(n int) Option {
	return func(s *Server) { s.pools[Background] = newPool(n) }
}

// WithInteractiveWorkers bounds how many Interactive-priority requests run
// concurrently. n <= 0 means unbounded, which is the default.
func WithInteractiveWorkers(n int) Option {
	return func(s *Server) { s.pools[Interactive] = newPool(n) }
}

// defaultBackgroundWorkers is WithBackgroundWorkers' default pool size.
// Background carries every workspace-wide/navigation query (definition,
// implementation, references, workspace/symbol, rename — see priority.go's
// doc), so a flat, small cap risks queuing one of them behind another that
// happens to be slow (a cold dependency closure check, a large references
// search) even though none of them contends for anything that makes them
// need to run one at a time — unlike Interactive, which is already
// unbounded by default. Tied to runtime.NumCPU (with a floor, so a
// single-core CI sandbox still gets a little headroom) rather than a flat
// constant, mirroring how a batch type-checker like gopls itself sizes its
// own worker pools: enough concurrency that a handful of queries in flight
// at once — several editor windows sharing one session, or a user jumping
// through a few dependency symbols in quick succession — never serialize
// behind each other purely because of this bound.
func defaultBackgroundWorkers() int {
	if n := runtime.NumCPU(); n > 4 {
		return n
	}
	return 4
}

// defaultDrainTimeout is drainTimeout's default, set by NewServer. See
// drain's own doc for why shutdown must not wait unboundedly.
const defaultDrainTimeout = 30 * time.Second

// defaultRequestTimeout is requestTimeout's default, set by NewServer.
const defaultRequestTimeout = 60 * time.Second

// requestTimeoutFromEnv reads requestTimeout's override, GOLANCE_REQUEST_TIMEOUT
// (a time.Duration string, e.g. "90s"), once at NewServer rather than per
// request: a request-scoped read would let the bound change mid-session for
// no benefit, since nothing in this process ever needs to change it after
// startup. An unset, empty, or unparseable value keeps
// defaultRequestTimeout; a parsed value <= 0 disables the bound (see
// requestTimeout's own doc).
func requestTimeoutFromEnv() time.Duration {
	v := os.Getenv("GOLANCE_REQUEST_TIMEOUT")
	if v == "" {
		return defaultRequestTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultRequestTimeout
	}
	return d
}

// NewServer constructs a Server. Register handlers with Handle and
// HandleNotification, then call Serve.
func NewServer(opts ...Option) *Server {
	s := &Server{
		logger:               log.Default(),
		requestHandlers:      make(map[string]requestReg),
		notificationHandlers: make(map[string]NotificationHandler),
		pools: map[Priority]*pool{
			Interactive: newPool(0),
			Background:  newPool(defaultBackgroundWorkers()),
		},
		queues:         make(map[string]*notifQueue),
		pending:        make(map[string]chan *message),
		drainTimeout:   defaultDrainTimeout,
		requestTimeout: requestTimeoutFromEnv(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handle registers h to answer requests for method, run on the given
// priority's worker pool. Call before Serve; Handle is not safe to call
// concurrently with Serve.
func (s *Server) Handle(method string, priority Priority, h RequestHandler) {
	s.requestHandlers[method] = requestReg{priority: priority, handler: h}
}

// HandleNotification registers h to handle notifications for method.
// Notifications for the same document URI (params.textDocument.uri) run
// serially and in arrival order across all registered methods; notifications
// without a document URI run serially against each other. Call before Serve.
func (s *Server) HandleNotification(method string, h NotificationHandler) {
	s.notificationHandlers[method] = h
}

// ExitError is returned by Serve when the client sends an "exit"
// notification. Code follows the LSP convention: 0 if "shutdown" was
// received first, 1 otherwise.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("rpc: exit notification received (code=%d)", e.Code)
}

// Serve reads Content-Length-framed JSON-RPC messages from r and dispatches
// them to registered handlers until r is exhausted or the client sends
// "exit". Responses and server-initiated notifications (see Notify) are
// written to w. Serve returns nil on a clean EOF from r (peer closed the
// pipe without sending exit) and an *ExitError after "exit". Before
// returning, Serve waits (bounded by drainTimeout — see drain) for every
// in-flight handler and Go-launched background goroutine to finish.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	s.conn = newConn(w)
	// s.ctx is deliberately independent of the per-request/notification ctx
	// dispatchRequest/dispatchNotification pass to handlers below (still
	// derived straight from the ctx parameter, unchanged): those already
	// have their own cancellation story (a request's own context.WithCancel
	// child, torn down when its handler returns; a notification's is never
	// individually canceled), and must keep running to completion here
	// exactly as before, not be preempted merely because Serve's read loop
	// is winding down. s.ctx exists only for Context/Go: detached
	// background work that has no other reason to stop.
	sessionCtx, cancel := context.WithCancel(ctx)
	s.ctx.Store(&sessionCtx)
	// cancel must run before drain below (defers run LIFO): canceling first
	// lets every Go-launched background goroutine observe it and return
	// promptly, so drain's own wait does not block on work that would
	// otherwise never stop on its own.
	defer s.drain()
	defer cancel()
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		raw, err := readFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("rpc: read frame: %w", err)
		}
		var m message
		if err := json.Unmarshal(raw, &m); err != nil {
			s.logger.Printf("rpc: malformed message: %v", err)
			continue
		}
		switch {
		case m.isRequest():
			s.dispatchRequest(ctx, &m)
		case m.isNotification():
			s.dispatchNotification(ctx, &m)
		case m.isResponse():
			s.dispatchResponse(&m)
		default:
			// Method=="" and ID==nil: not a request, notification, or
			// response by the JSON-RPC 2.0 envelope rules message.go's
			// isRequest/isNotification/isResponse implement. No real client
			// sends this; log it rather than dropping it with no trace.
			s.logger.Printf("rpc: dropping frame matching no request/notification/response shape: %s", raw)
		}
		if lifecycleState(s.state.Load()) == stateExited {
			return s.exitErr
		}
	}
}

// drain waits for wg — every in-flight request/notification handler plus
// every Go-launched background goroutine — to finish, bounded by
// drainTimeout. A handler that never returns (a genuine bug, or a client
// process that died mid-request with nothing left to observe cancellation)
// would otherwise block Serve from ever returning: the process this Serve
// call belongs to is already on its way out by the time drain runs, so
// abandoning a wedged handler here trades a clean wait for guaranteed
// forward progress instead of orphaning the whole process indefinitely.
func (s *Server) drain() {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(s.drainTimeout):
		s.logger.Printf("rpc: shutdown drain timed out after %s; abandoning in-flight handlers", s.drainTimeout)
	}
}

// Context returns the context bound to this Serve call's own lifetime: a
// child of the ctx passed to Serve, canceled once Serve returns (client
// "exit", EOF, or a read error) but before Serve's own shutdown-time
// drain completes. A request handler's own ctx parameter is instead a
// per-request child canceled the moment that handler returns (see
// dispatchRequest), so it is the wrong choice for detached background
// work started from a request handler that must outlive the request
// itself — use Go instead, which supplies this context automatically.
// Before Serve has been called, Context returns context.Background(), so
// code that may run in a test harness without a real Serve session (e.g.
// exercising a handler directly) still gets a valid, if uncancelable,
// context rather than a nil one.
func (s *Server) Context() context.Context {
	if p := s.ctx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// Go runs fn in its own goroutine, passed Context() and tracked by wg the
// same way an in-flight request/notification handler is — so Serve's
// shutdown-time drain waits for it, bounded by drainTimeout, instead of
// abandoning it immediately — for detached background work a handler
// starts that must outlive the call that started it (e.g. launching the
// indexer subprocess, a debounced reindex) but should still stop once the
// session itself ends.
//
// A panic in fn is recovered and logged with a stack trace, mirroring
// callRequestHandler's and callNotificationHandler's own handler-panic
// recovery, rather than left to unwind: unlike a request or notification,
// detached background work has no dispatch-loop caller left to fail
// gracefully by the time fn runs, so an unrecovered panic here would take
// the whole process down instead of just this one piece of background
// work. wg.Done still runs either way, so a panicking fn cannot wedge
// Serve's shutdown-time drain.
func (s *Server) Go(fn func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.logger.Printf("rpc: panic in background task: %v\n%s", r, debug.Stack())
			}
		}()
		fn(s.Context())
	}()
}

func (s *Server) dispatchRequest(ctx context.Context, m *message) {
	reg, ok := s.requestHandlers[m.Method]
	st := lifecycleState(s.state.Load())
	switch {
	case !ok:
		s.respondError(m.ID, NewError(methodNotFoundCode, "method not found: "+m.Method))
		return
	case st == stateUninitialized && m.Method != "initialize":
		s.respondError(m.ID, NewError(serverNotInitializedCode, "server not initialized"))
		return
	case st == stateShuttingDown || st == stateExited:
		s.respondError(m.ID, NewError(invalidRequestCode, "server is shutting down"))
		return
	}
	switch m.Method {
	case "initialize":
		s.state.Store(int32(stateInitialized))
	case "shutdown":
		s.state.Store(int32(stateShuttingDown))
	}

	// Only a Background request is ever given a deadline: Interactive
	// carries latency-sensitive requests (completion, hover) a client is
	// actively waiting on, which must run to completion or be canceled by
	// the client itself, not cut off by a server-side guess at how long is
	// too long.
	var reqCtx context.Context
	var cancel context.CancelFunc
	if reg.priority == Background && s.requestTimeout > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, s.requestTimeout)
	} else {
		reqCtx, cancel = context.WithCancel(ctx)
	}
	idKey := string(m.ID)
	id := append(json.RawMessage(nil), m.ID...)
	method := m.Method
	params := m.Params
	s.cancels.register(idKey, cancel)
	s.wg.Add(1)

	// Joined here, synchronously in the read loop — not inside the pool
	// goroutine runRequest runs in — so barrier's position in its document's
	// notifQueue reflects the wire order this request actually arrived in:
	// any same-document notification (didOpen/didChange/didSave) already
	// dispatched ahead of it is guaranteed to run first (see
	// notifQueue.join's doc for why dispatchRequest and dispatchNotification
	// otherwise have no ordering guarantee between them at all).
	barrier := s.queueFor(notificationQueueKey(params)).join()

	s.pools[reg.priority].run(func() {
		s.runRequest(reqCtx, cancel, barrier, idKey, id, method, params, reg.handler)
	})
}

// runRequest is dispatchRequest's pool-dispatched half: it waits for
// barrier (see dispatchRequest's own doc), invokes handler, and writes the
// resulting response — split out of dispatchRequest itself to keep that
// function's own job (validating and preparing one request) separate from
// this one (actually running it).
//
// The wait for barrier is unconditional, not raced against reqCtx.Done():
// handler must always be invoked at least once, exactly as before barrier
// existed — a $/cancelRequest landing while still waiting must not skip
// calling it, or TestCancelRequestDoesNotDiscardResultTheHandlerAlreadyComputed's
// own invariant (a handler that already started must still get to finish and
// have its result honored) would have no chance to hold in the first place.
// Every notification handler in this codebase is fast and non-blocking (see
// notifQueue's own doc), so this wait is bounded in practice; a handler that
// itself watches ctx remains the only mechanism for bounding how long a
// request runs once started, exactly as callRequestHandler's own doc
// describes.
func (s *Server) runRequest(reqCtx context.Context, cancel context.CancelFunc, barrier <-chan struct{}, idKey string, id json.RawMessage, method string, params json.RawMessage, handler RequestHandler) {
	defer s.wg.Done()
	defer s.cancels.unregister(idKey)
	defer cancel()
	<-barrier
	result, err := s.callRequestHandler(reqCtx, method, handler, params)
	switch {
	// Checking err here, not reqCtx.Err(), matters: a $/cancelRequest
	// for this id can call cancel (and so close reqCtx.Done()) at any
	// point, including in the narrow window after the handler already
	// returned a valid result. Basing the decision on reqCtx.Err()
	// would make that race discard an already-computed answer. err
	// reflects what the handler itself observed — a handler that
	// notices ctx.Done() is expected to return ctx.Err() (the
	// convention every handler in this codebase follows) — so a
	// cancellation that arrives too late for the handler to see it
	// correctly has no effect on the response.
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		// reqCtx.Err() (not err) tells apart the two ways this branch is
		// reached: an explicit $/cancelRequest calls cancel directly,
		// leaving reqCtx.Err() == context.Canceled even when reqCtx was
		// built with WithTimeout, while only the deadline itself
		// actually firing ever sets it to DeadlineExceeded.
		if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
			s.logger.Printf("rpc: request %s (id=%s) exceeded its %s timeout", method, idKey, s.requestTimeout)
		}
		s.respondError(id, NewError(requestCancelledCode, "request cancelled"))
	case err != nil:
		s.respondError(id, toWireError(err))
	default:
		s.respondResult(id, result)
	}
}

func (s *Server) dispatchNotification(ctx context.Context, m *message) {
	switch m.Method {
	case "$/cancelRequest":
		s.handleCancelRequest(m.Params)
		return
	case "exit":
		s.handleExit()
		return
	}
	switch lifecycleState(s.state.Load()) {
	case stateUninitialized, stateShuttingDown, stateExited:
		return // dropped per LSP lifecycle rules
	}
	handler, ok := s.notificationHandlers[m.Method]
	if !ok {
		return
	}
	method, params := m.Method, m.Params
	s.wg.Add(1)
	s.queueFor(notificationQueueKey(params)).push(func() {
		defer s.wg.Done()
		s.callNotificationHandler(ctx, method, handler, params)
	})
}

// callRequestHandler invokes h, recovering a panic so a bug in one handler
// fails only this request instead of crashing the whole server. The panic
// value and a stack trace are logged server-side; the client only ever sees
// a generic InternalError, never the panic details.
func (s *Server) callRequestHandler(ctx context.Context, method string, h RequestHandler, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("rpc: panic in request handler %s: %v\n%s", method, r, debug.Stack())
			err = &Error{Code: internalErrorCode, Message: "internal error"}
		}
	}()
	return h(ctx, params)
}

// callNotificationHandler invokes h, recovering a panic so it can't crash
// the whole server or wedge this notification's per-document queue (an
// unrecovered panic would exit notifQueue.drain's loop without resetting
// q.running, stalling every later notification for the same key). Per the
// JSON-RPC notification contract there is no response channel, so both a
// returned error and a recovered panic are only logged.
func (s *Server) callNotificationHandler(ctx context.Context, method string, h NotificationHandler, params json.RawMessage) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("rpc: panic in notification handler %s: %v\n%s", method, r, debug.Stack())
		}
	}()
	if err := h(ctx, params); err != nil {
		s.logger.Printf("rpc: notification %s: %v", method, err)
	}
}

// dispatchResponse routes a response to one of our own server-initiated
// requests (see Request) to the caller awaiting it, keyed by id. A response
// with no matching pending entry (already delivered, or Request's caller
// already gave up on ctx) is silently dropped.
func (s *Server) dispatchResponse(m *message) {
	key := string(m.ID)
	s.pendingMu.Lock()
	ch, ok := s.pending[key]
	delete(s.pending, key)
	s.pendingMu.Unlock()
	if !ok {
		return
	}
	ch <- m
}

func (s *Server) handleCancelRequest(params json.RawMessage) {
	var cp struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(params, &cp) != nil || cp.ID == nil {
		return
	}
	s.cancels.cancel(string(cp.ID))
}

func (s *Server) handleExit() {
	code := 1
	if lifecycleState(s.state.Load()) == stateShuttingDown {
		code = 0
	}
	s.exitErr = &ExitError{Code: code}
	s.state.Store(int32(stateExited))
}

// notificationQueueKey extracts params.textDocument.uri, if present, to key
// the per-document serial notification queue. Notifications without a
// textDocument (e.g. didChangeConfiguration) share the "" queue.
func notificationQueueKey(params json.RawMessage) string {
	var dp struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if json.Unmarshal(params, &dp) != nil {
		return ""
	}
	return dp.TextDocument.URI
}

func (s *Server) queueFor(key string) *notifQueue {
	s.queuesMu.Lock()
	defer s.queuesMu.Unlock()
	q, ok := s.queues[key]
	if !ok {
		q = &notifQueue{}
		s.queues[key] = q
	}
	return q
}

func (s *Server) respondResult(id json.RawMessage, result any) {
	b, err := protocol.Marshal(result)
	if err != nil {
		s.respondError(id, NewError(internalErrorCode, err.Error()))
		return
	}
	if err := s.writeMessage(&message{JSONRPC: jsonrpcVersion, ID: id, Result: b}); err != nil {
		s.logger.Printf("rpc: write result for %v: %v", id, err)
	}
}

func (s *Server) respondError(id json.RawMessage, e *Error) {
	if err := s.writeMessage(&message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &wireError{Code: e.Code, Message: e.Message, Data: e.Data},
	}); err != nil {
		s.logger.Printf("rpc: write error for %v: %v", id, err)
	}
}

// Notify sends a server-initiated notification to the client, marshaling
// params with the LSP-conformant encoder (go.lsp.dev/protocol.Marshal) so
// union-typed payloads round-trip correctly.
func (s *Server) Notify(method string, params any) error {
	b, err := protocol.Marshal(params)
	if err != nil {
		return fmt.Errorf("rpc: marshal notify params for %s: %w", method, err)
	}
	return s.writeMessage(&message{JSONRPC: jsonrpcVersion, Method: method, Params: b})
}

// Request sends a server-initiated JSON-RPC request to the client and
// blocks until it responds or ctx is done. This is the mechanism for the
// small set of server-to-client requests LSP defines (e.g.
// client/registerCapability) that need the client's response; server-
// initiated notifications that don't use Notify instead.
func (s *Server) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b, err := protocol.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("rpc: marshal request params for %s: %w", method, err)
	}
	id := s.nextID.Add(1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("rpc: marshal request id: %w", err)
	}
	key := string(idJSON)
	// Buffered by 1 so dispatchResponse's send never blocks even if this
	// call has already given up (ctx done) by the time the response
	// arrives; pendingMu below makes the lookup-and-delete in
	// dispatchResponse and the delete in this func's defer mutually
	// exclusive, so at most one of them ever sends/reads on ch.
	ch := make(chan *message, 1)
	s.pendingMu.Lock()
	s.pending[key] = ch
	s.pendingMu.Unlock()
	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	}()

	if err := s.writeMessage(&message{JSONRPC: jsonrpcVersion, ID: idJSON, Method: method, Params: b}); err != nil {
		return nil, fmt.Errorf("rpc: write request %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, &Error{Code: resp.Error.Code, Message: resp.Error.Message, Data: resp.Error.Data}
		}
		return resp.Result, nil
	}
}

func (s *Server) writeMessage(m *message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("rpc: marshal message: %w", err)
	}
	if s.conn == nil {
		return fmt.Errorf("rpc: server not serving")
	}
	return s.conn.write(b)
}
