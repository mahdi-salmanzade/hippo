package mcp

import (
	"context"
	"errors"
	"sync"
)

// transport is the internal abstraction over stdio and Streamable
// HTTP. Client uses it for both request/response and lifecycle
// signals. Implementations are created fresh on each (re)connect; the
// Client owns the outer lifecycle.
type transport interface {
	// Send issues a JSON-RPC request and returns the matching
	// response, or an error if the transport fails or ctx expires.
	// For notifications, callers pass an ID-less message and should
	// expect a nil response.
	Send(ctx context.Context, req *jsonrpcMessage) (*jsonrpcMessage, error)

	// Notify sends an ID-less notification and returns once the bytes
	// have been written. Servers do not respond to notifications.
	Notify(ctx context.Context, req *jsonrpcMessage) error

	// Disconnected returns a channel closed when the transport detects
	// its underlying connection has died. Used by the reconnect loop.
	Disconnected() <-chan struct{}

	// Close terminates the transport and releases its resources.
	// Idempotent.
	Close() error
}

// pendingRegistry matches inbound responses to the goroutines waiting
// for them by ID. stdio and HTTP transports both reuse it.
type pendingRegistry struct {
	mu      sync.Mutex
	pending map[string]chan *jsonrpcMessage
	closed  bool
}

func newPendingRegistry() *pendingRegistry {
	return &pendingRegistry{pending: make(map[string]chan *jsonrpcMessage)}
}

// register reserves a slot for id and returns the channel that will
// receive the matching response. The registry refuses new
// registrations after Fail/CloseAll, returning a closed error so the
// caller fails fast instead of blocking forever.
func (p *pendingRegistry) register(id string) (<-chan *jsonrpcMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errTransportClosed
	}
	if _, dup := p.pending[id]; dup {
		return nil, errDuplicateID
	}
	ch := make(chan *jsonrpcMessage, 1)
	p.pending[id] = ch
	return ch, nil
}

// cancel removes the slot and returns the channel; used when the
// caller's ctx expires before a response arrives so the reader
// goroutine does not accumulate zombie entries.
func (p *pendingRegistry) cancel(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending, id)
}

// deliver routes an inbound response to the waiting channel. Unknown
// ids (response arrived after ctx cancel, or a notification the
// server sent back with an id) are dropped silently — the response
// would have been discarded anyway.
func (p *pendingRegistry) deliver(msg *jsonrpcMessage) bool {
	id := idString(msg.ID)
	if id == "" {
		return false
	}
	p.mu.Lock()
	ch, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	// Non-blocking send: the channel is buffered size 1 and only
	// consumed once by register().
	select {
	case ch <- msg:
	default:
	}
	return true
}

// failAll closes every pending channel with a nil value, unblocking
// callers whose transport died mid-request. Subsequent register calls
// return errTransportClosed.
func (p *pendingRegistry) failAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for id, ch := range p.pending {
		close(ch)
		delete(p.pending, id)
	}
}

var (
	errTransportClosed = errors.New("mcp: transport closed")
	errDuplicateID     = errors.New("mcp: duplicate request id")
)
