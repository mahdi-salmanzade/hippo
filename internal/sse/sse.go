// Package sse is a minimal Server-Sent Events scanner used by provider
// streaming implementations.
//
// This package is internal: it is not part of hippo's public API. It
// wraps bufio.Scanner with the SSE framing rules (event/data/id/retry
// fields, blank-line-terminated events) and nothing more.
package sse

import (
	"bufio"
	"io"
)

// Event is one SSE event, in decoded form.
type Event struct {
	// ID is the optional "id:" field.
	ID string
	// Event is the optional "event:" field; empty string means the
	// default event type.
	Event string
	// Data is the concatenation of all "data:" lines, newline-joined.
	Data []byte
}

// Scanner reads SSE events from an io.Reader.
type Scanner struct {
	r *bufio.Reader
	// TODO: carry partial event state across Scan calls; support the
	// "retry:" field if needed.
}

// NewScanner wraps r with a 1 MiB buffered reader.
func NewScanner(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewReaderSize(r, 1<<20)}
}

// Scan reads the next event. It returns io.EOF when the stream ends
// cleanly.
func (s *Scanner) Scan() (Event, error) {
	// TODO: parse field lines until a blank line terminates the event;
	// skip comments (lines beginning with ':').
	return Event{}, io.EOF
}
