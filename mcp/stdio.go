package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
)

// stdioMaxLine is the largest single JSON-RPC message hippo will
// accept over stdio. Tool results with moderate-sized content fit
// comfortably; servers streaming multi-megabyte blobs exceed this
// and return bufio.ErrTooLong, which the reader surfaces as
// disconnect + reconnect.
const stdioMaxLine = 1 << 20 // 1 MiB

// stdioTransport speaks MCP over a subprocess's stdin/stdout using
// newline-delimited JSON. Writes are serialized by writeMu; reads
// happen in the reader goroutine and dispatch to pendingRegistry.
type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	writeMu sync.Mutex

	pending *pendingRegistry

	log *slog.Logger

	// dead is closed exactly once via deadOne. markDead and Close both
	// race to close it; the Once ensures only one wins.
	dead     chan struct{}
	deadOne  sync.Once
	closed   chan struct{}
	closeOne sync.Once
}

// startStdioTransport spawns the command and starts the reader
// goroutine. Returns an error if the subprocess cannot be started.
// The caller is responsible for running the initialize handshake
// after start.
func startStdioTransport(ctx context.Context, command []string, log *slog.Logger) (*stdioTransport, error) {
	if len(command) == 0 {
		return nil, errors.New("mcp: stdio command is empty")
	}
	// exec.CommandContext kills the subprocess when ctx is cancelled,
	// which is what Close() wants when the user cancels at shutdown.
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("mcp: start %v: %w", command, err)
	}

	t := &stdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		log:     log,
		pending: newPendingRegistry(),
		dead:    make(chan struct{}),
		closed:  make(chan struct{}),
	}

	go t.readLoop()
	go t.stderrLoop(stderr)
	go t.waitLoop()

	return t, nil
}

func (t *stdioTransport) Disconnected() <-chan struct{} { return t.dead }

// Close terminates the subprocess and unblocks any in-flight Send
// calls. Idempotent.
func (t *stdioTransport) Close() error {
	t.closeOne.Do(func() {
		close(t.closed)
		_ = t.stdin.Close()
		// Killing the process also causes the read goroutine to hit EOF.
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		t.pending.failAll()
		t.deadOne.Do(func() { close(t.dead) })
	})
	return nil
}

// Send writes req, registers a pending slot keyed by its id, and
// waits for the matching response. ctx cancellation cancels only
// this Send; the transport itself stays up unless the ctx is the
// Client's root ctx.
func (t *stdioTransport) Send(ctx context.Context, req *jsonrpcMessage) (*jsonrpcMessage, error) {
	id := idString(req.ID)
	if id == "" {
		return nil, errors.New("mcp: stdio Send: request has no id")
	}
	ch, err := t.pending.register(id)
	if err != nil {
		return nil, err
	}

	if err := t.writeMessage(req); err != nil {
		t.pending.cancel(id)
		t.markDead()
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errTransportClosed
		}
		return resp, nil
	case <-ctx.Done():
		t.pending.cancel(id)
		return nil, ctx.Err()
	case <-t.dead:
		return nil, errTransportClosed
	}
}

// Notify writes an ID-less message.
func (t *stdioTransport) Notify(ctx context.Context, req *jsonrpcMessage) error {
	if err := t.writeMessage(req); err != nil {
		t.markDead()
		return err
	}
	return nil
}

// writeMessage serialises one JSON-RPC message and appends the
// newline delimiter. The mutex ensures concurrent Sends from
// different goroutines interleave at message boundaries rather than
// byte boundaries.
func (t *stdioTransport) writeMessage(msg *jsonrpcMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}
	body = append(body, '\n')

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(body); err != nil {
		return fmt.Errorf("mcp: stdin write: %w", err)
	}
	return nil
}

// readLoop reads JSON-RPC messages from stdout and dispatches each
// to the pending registry. Exits on EOF or parse failure; either way
// the transport is considered dead.
func (t *stdioTransport) readLoop() {
	defer t.markDead()
	scanner := bufio.NewScanner(t.stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), stdioMaxLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// Content-Length framing (legacy): if a server uses the
		// LSP-style framing, we're not set up to parse it and the
		// first line will be "Content-Length: N". Flag and drop;
		// the server should negotiate the modern delimiter on its
		// own side.
		if bytes.HasPrefix(line, []byte("Content-Length:")) {
			t.log.Warn("mcp: stdio server sent Content-Length framing; hippo requires newline-delimited JSON",
				"line", string(line))
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			t.log.Warn("mcp: stdio parse failed",
				"err", err, "line", truncate(string(line), 200))
			continue
		}
		if msg.ID == nil && msg.Method != "" {
			// Server notification. We don't subscribe to any in
			// v0.1.0 — drop and log at Debug.
			t.log.Debug("mcp: stdio ignoring notification", "method", msg.Method)
			continue
		}
		if !t.pending.deliver(&msg) {
			t.log.Debug("mcp: stdio response has no waiter", "id", idString(msg.ID))
		}
	}
	if err := scanner.Err(); err != nil {
		t.log.Warn("mcp: stdio read error", "err", err)
	}
}

// stderrLoop forwards subprocess stderr lines to the logger at Debug.
// MCP servers commonly use stderr for startup logging; surfacing it
// lives with the rest of hippo's slog output.
func (t *stdioTransport) stderrLoop(r io.ReadCloser) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 16*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		t.log.Debug("mcp: stdio server stderr", "line", line)
	}
}

// waitLoop reaps the subprocess so the OS does not accumulate
// zombies, and marks the transport dead on exit.
func (t *stdioTransport) waitLoop() {
	err := t.cmd.Wait()
	if err != nil {
		t.log.Debug("mcp: stdio subprocess exited", "err", err)
	}
	t.markDead()
}

func (t *stdioTransport) markDead() {
	t.deadOne.Do(func() { close(t.dead) })
	t.pending.failAll()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
