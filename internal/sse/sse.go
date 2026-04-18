// Package sse is a minimal Server-Sent Events scanner used by provider
// streaming implementations.
//
// This package is internal: it is not part of hippo's public API. It
// implements just enough of the SSE framing rules (event/data/id
// fields, blank-line-terminated events, comment lines, CRLF and LF
// line endings) to parse the event streams Anthropic, OpenAI, and
// future providers emit from their /messages and /responses endpoints.
// retry-field support is deliberately omitted — no hippo provider
// uses it, and the http.Client reconnect-on-failure shape isn't how
// hippo handles mid-stream failures anyway (see provider docs).
//
// Cancellation is driven by closing the underlying io.Reader; Next
// checks ctx.Err() at the top of each call as a cheap fast-path, but
// once inside a blocking read only closing the body unblocks it. The
// provider-side goroutine owns the body-close-on-ctx pattern.
package sse

import (
	"bufio"
	"bytes"
	"context"
	"io"
)

// defaultBufSize is the initial and max buffer size for bufio.Scanner.
// 1 MiB comfortably fits the longest events we've seen (multi-kilobyte
// tool_use input JSON from Anthropic, full response.completed payloads
// from OpenAI with 16k+ token usage metadata).
const defaultBufSize = 1 << 20

// Event is one SSE event, in decoded form. The zero Event is the
// default event type with no data.
type Event struct {
	// ID is the optional "id:" field. Empty when not present.
	ID string
	// Event is the optional "event:" field; empty string means the
	// default event type. Both Anthropic and OpenAI populate this,
	// so most hippo code switches on Event rather than parsing Data.
	Event string
	// Data is the concatenation of all "data:" lines in the event,
	// joined by a single "\n" per the SSE spec. Callers json.Unmarshal
	// this directly.
	Data []byte
}

// Scanner reads SSE events from an io.Reader. Not concurrency-safe;
// callers own the scanner and must serialise Next calls.
type Scanner struct {
	scanner *bufio.Scanner
	// fieldBuf is the per-event accumulator for data lines. Reused
	// across Next calls to avoid a per-event allocation on the hot
	// read path.
	dataBuf bytes.Buffer
}

// NewScanner wraps r with a 1 MiB line buffer. The buffer size is fixed
// at construction; if a provider ever emits an event larger than that,
// the scanner returns bufio.ErrTooLong and the caller surfaces it as a
// stream failure. 1 MiB is plenty for every event shape hippo has
// observed in practice; bump the default or add a WithBufSize option
// if a future provider needs it.
func NewScanner(r io.Reader) *Scanner {
	s := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	s.Buffer(buf, defaultBufSize)
	return &Scanner{scanner: s}
}

// Next reads the next event from the stream. Returns io.EOF when the
// stream ends cleanly with no pending event data. A context that is
// already cancelled short-circuits with ctx.Err() before the scanner
// touches the underlying reader; mid-read cancellation must be driven
// by closing the reader from a sibling goroutine.
func (s *Scanner) Next(ctx context.Context) (*Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var ev Event
	s.dataBuf.Reset()
	haveAnyField := false

	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		// Handle both LF and CRLF: bufio.Scanner's default SplitFunc
		// strips the trailing \n but leaves a \r behind on Windows-
		// style line endings.
		if n := len(line); n > 0 && line[n-1] == '\r' {
			line = line[:n-1]
		}

		// Blank line terminates the event. Only emit if we accumulated
		// at least one field — successive blank lines, or a stream
		// that starts with a blank, should not yield an empty event.
		if len(line) == 0 {
			if !haveAnyField {
				continue
			}
			ev.Data = append([]byte(nil), s.dataBuf.Bytes()...)
			return &ev, nil
		}

		// Comment line: ":" prefix. Whole line is ignored per spec.
		if line[0] == ':' {
			continue
		}

		// Field line. Per SSE spec: field name up to first ':',
		// value is everything after (with one optional leading
		// space stripped). Lines with no ':' have the whole line as
		// the field name and an empty value.
		name, value := splitField(line)
		haveAnyField = true

		switch string(name) {
		case "event":
			ev.Event = string(value)
		case "id":
			ev.ID = string(value)
		case "data":
			if s.dataBuf.Len() > 0 {
				s.dataBuf.WriteByte('\n')
			}
			s.dataBuf.Write(value)
		case "retry":
			// Ignored: see package doc.
		default:
			// Unknown fields are silently dropped per spec.
		}
	}

	if err := s.scanner.Err(); err != nil {
		return nil, err
	}

	// Graceful end of stream. If the stream ended without a trailing
	// blank line but we have buffered field data, emit one last event
	// — some providers omit the final terminator.
	if haveAnyField {
		ev.Data = append([]byte(nil), s.dataBuf.Bytes()...)
		return &ev, nil
	}
	return nil, io.EOF
}

// splitField splits an SSE field line into name and value. One
// leading space after the colon is stripped per spec; additional
// whitespace is preserved.
func splitField(line []byte) (name, value []byte) {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 {
		return line, nil
	}
	name = line[:colon]
	value = line[colon+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return name, value
}
