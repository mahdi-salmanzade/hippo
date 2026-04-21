package sse

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestScannerSimpleEvent(t *testing.T) {
	const stream = "event: message\n" +
		"data: hello\n" +
		"\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "message" {
		t.Errorf("Event = %q, want %q", ev.Event, "message")
	}
	if string(ev.Data) != "hello" {
		t.Errorf("Data = %q, want %q", ev.Data, "hello")
	}

	_, err = s.Next(context.Background())
	if err != io.EOF {
		t.Errorf("second Next err = %v, want io.EOF", err)
	}
}

func TestScannerMultiLineData(t *testing.T) {
	// Per SSE spec, multiple data: lines in one event concatenate
	// with "\n" between them.
	const stream = "data: line one\n" +
		"data: line two\n" +
		"data: line three\n" +
		"\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	want := "line one\nline two\nline three"
	if string(ev.Data) != want {
		t.Errorf("Data = %q, want %q", ev.Data, want)
	}
}

func TestScannerCommentsIgnored(t *testing.T) {
	const stream = ": this is a comment\n" +
		": another comment\n" +
		"event: ping\n" +
		"data: {}\n" +
		"\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "ping" {
		t.Errorf("Event = %q, want %q (comments should not disturb parsing)", ev.Event, "ping")
	}
}

func TestScannerHandlesCRLF(t *testing.T) {
	const stream = "event: msg\r\n" +
		"data: windows-style\r\n" +
		"\r\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "msg" {
		t.Errorf("Event = %q, want %q", ev.Event, "msg")
	}
	if string(ev.Data) != "windows-style" {
		t.Errorf("Data = %q, want %q", ev.Data, "windows-style")
	}
}

func TestScannerLargeEvent(t *testing.T) {
	// 300 KiB of data in a single event - larger than bufio.Scanner's
	// default 64 KiB max but well within our 1 MiB override. This
	// pins the buffer-size behaviour; a regression that reverts to the
	// default would fail here with bufio.ErrTooLong.
	const size = 300 * 1024
	big := strings.Repeat("x", size)

	stream := "event: fat\n" +
		"data: " + big + "\n" +
		"\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next on %d-byte event: %v", size, err)
	}
	if len(ev.Data) != size {
		t.Errorf("Data length = %d, want %d", len(ev.Data), size)
	}
}

func TestScannerContextCancelBeforeRead(t *testing.T) {
	// A cancelled context must short-circuit before touching the
	// reader. Mid-read cancellation is the caller's job (close the
	// body); the fast-path at top of Next is what we verify here.
	const stream = "event: x\ndata: y\n\n"
	s := NewScanner(strings.NewReader(stream))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Next(ctx)
	if err == nil {
		t.Fatal("Next returned nil error after ctx cancel, want ctx.Err()")
	}
	if err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestScannerNoTrailingBlankLine(t *testing.T) {
	// Providers occasionally omit the final blank line. The scanner
	// should still surface that last event rather than dropping it.
	const stream = "event: last\ndata: bye\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "last" || string(ev.Data) != "bye" {
		t.Errorf("event = %+v, want {Event=last, Data=bye}", ev)
	}
	if _, err := s.Next(context.Background()); err != io.EOF {
		t.Errorf("follow-up Next err = %v, want io.EOF", err)
	}
}

func TestScannerMultipleEvents(t *testing.T) {
	const stream = "event: first\ndata: one\n\n" +
		"event: second\ndata: two\n\n" +
		"event: third\ndata: three\n\n"
	s := NewScanner(strings.NewReader(stream))

	want := []struct{ name, data string }{
		{"first", "one"}, {"second", "two"}, {"third", "three"},
	}
	for i, w := range want {
		ev, err := s.Next(context.Background())
		if err != nil {
			t.Fatalf("Next #%d: %v", i, err)
		}
		if ev.Event != w.name || string(ev.Data) != w.data {
			t.Errorf("event #%d = %+v, want {%s, %s}", i, ev, w.name, w.data)
		}
	}
	if _, err := s.Next(context.Background()); err != io.EOF {
		t.Errorf("final err = %v, want io.EOF", err)
	}
}

func TestScannerIDField(t *testing.T) {
	const stream = "id: 42\n" +
		"event: checkpoint\n" +
		"data: ok\n" +
		"\n"
	s := NewScanner(strings.NewReader(stream))

	ev, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.ID != "42" {
		t.Errorf("ID = %q, want %q", ev.ID, "42")
	}
}

func TestScannerFieldWithoutSpace(t *testing.T) {
	// Per spec, the single space after the colon is optional. Both
	// "data:foo" and "data: foo" must yield "foo".
	const stream = "data:nospace\n\n" +
		"data: withspace\n\n"
	s := NewScanner(strings.NewReader(stream))

	first, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next 1: %v", err)
	}
	if string(first.Data) != "nospace" {
		t.Errorf("first Data = %q, want nospace", first.Data)
	}
	second, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next 2: %v", err)
	}
	if string(second.Data) != "withspace" {
		t.Errorf("second Data = %q, want withspace", second.Data)
	}
}
