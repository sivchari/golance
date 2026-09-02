package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"

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
		queues:  make(map[string]*notifQueue),
		pending: make(map[string]chan *message),
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
// pipe without sending exit) and an *ExitError after "exit".
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
	// cancel must run before wg.Wait below (defers run LIFO): canceling
	// first lets every Go-launched background goroutine observe it and
	// return promptly, so wg.Wait — which also drains those — does not
	// block on work that would otherwise never stop on its own.
	defer s.wg.Wait()
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
		}
		if lifecycleState(s.state.Load()) == stateExited {
			return s.exitErr
		}
	}
}

// Context returns the context bound to this Serve call's own lifetime: a
// child of the ctx passed to Serve, canceled once Serve returns (client
// "exit", EOF, or a read error) but before Serve's own shutdown-time
// wg.Wait completes. A request handler's own ctx parameter is instead a
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
// shutdown-time drain waits (briefly) for it instead of abandoning it —
// for detached background work a handler starts that must outlive the
// call that started it (e.g. launching the indexer subprocess, a
// debounced reindex) but should still stop once the session itself ends.
func (s *Server) Go(fn func(ctx context.Context)) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
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

	reqCtx, cancel := context.WithCancel(ctx)
	idKey := string(m.ID)
	id := append(json.RawMessage(nil), m.ID...)
	method := m.Method
	params := m.Params
	s.cancels.register(idKey, cancel)
	s.wg.Add(1)
	s.pools[reg.priority].run(func() {
		defer s.wg.Done()
		defer s.cancels.unregister(idKey)
		defer cancel()
		result, err := s.callRequestHandler(reqCtx, method, reg.handler, params)
		switch {
		case reqCtx.Err() != nil:
			s.respondError(id, NewError(requestCancelledCode, "request cancelled"))
		case err != nil:
			s.respondError(id, toWireError(err))
		default:
			s.respondResult(id, result)
		}
	})
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
