package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

	// wg tracks in-flight request and notification handler goroutines so
	// Serve can wait for their responses/side effects to complete before
	// returning, instead of racing the write of a still-running handler
	// against the caller closing the connection.
	wg sync.WaitGroup

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
// concurrently. n <= 0 means unbounded. The default is 4.
func WithBackgroundWorkers(n int) Option {
	return func(s *Server) { s.pools[Background] = newPool(n) }
}

// WithInteractiveWorkers bounds how many Interactive-priority requests run
// concurrently. n <= 0 means unbounded, which is the default.
func WithInteractiveWorkers(n int) Option {
	return func(s *Server) { s.pools[Interactive] = newPool(n) }
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
			Background:  newPool(4),
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
	defer s.wg.Wait()
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
	params := m.Params
	s.cancels.register(idKey, cancel)
	s.wg.Add(1)
	s.pools[reg.priority].run(func() {
		defer s.wg.Done()
		defer s.cancels.unregister(idKey)
		defer cancel()
		result, err := reg.handler(reqCtx, params)
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
		if err := handler(ctx, params); err != nil {
			s.logger.Printf("rpc: notification %s: %v", method, err)
		}
	})
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
