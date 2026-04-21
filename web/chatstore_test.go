package web

import (
	"context"
	"strings"
	"testing"
)

func newTestChatStore(t *testing.T) *ChatStore {
	t.Helper()
	s, err := NewChatStore(":memory:")
	if err != nil {
		t.Fatalf("NewChatStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestChatStoreCreateAndGet(t *testing.T) {
	s := newTestChatStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == "" {
		t.Fatal("Create returned empty id")
	}

	msgs, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get on fresh session: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("fresh session should have 0 messages; got %d", len(msgs))
	}
}

func TestChatStoreAppendAndGet(t *testing.T) {
	s := newTestChatStore(t)
	ctx := context.Background()
	id, err := s.Create(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Append(ctx, id, "user", "what is hippo?"); err != nil {
		t.Fatalf("Append user: %v", err)
	}
	if err := s.Append(ctx, id, "assistant", "hippo is a local-first LLM router."); err != nil {
		t.Fatalf("Append assistant: %v", err)
	}

	msgs, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "what is hippo?" {
		t.Errorf("msg[0] = %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("msg[1].Role = %q, want assistant", msgs[1].Role)
	}
}

func TestChatStoreAutoTitleFromFirstUserTurn(t *testing.T) {
	s := newTestChatStore(t)
	ctx := context.Background()
	id, _ := s.Create(ctx)

	// First user turn sets the title.
	if err := s.Append(ctx, id, "user", "Explain how the yaml router picks a model for a task"); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, 10)
	if len(list) != 1 {
		t.Fatalf("expected 1 session; got %d", len(list))
	}
	if !strings.Contains(list[0].Title, "Explain how") {
		t.Errorf("title = %q; want to start with 'Explain how'", list[0].Title)
	}

	// Second user turn must NOT overwrite the title.
	if err := s.Append(ctx, id, "user", "follow-up"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.List(ctx, 10)
	if strings.Contains(list[0].Title, "follow-up") {
		t.Errorf("title got overwritten on second turn: %q", list[0].Title)
	}
}

func TestChatStoreTitleTruncatesLongInput(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := titleFromUserTurn(long)
	// 60 char cap + 3-byte UTF-8 ellipsis = 63 bytes max.
	if len(got) > 63 {
		t.Errorf("title byte len = %d; want <= 63", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("title should end with ellipsis; got %q", got)
	}
}

func TestChatStoreTitleCollapsesNewlines(t *testing.T) {
	got := titleFromUserTurn("first line\nsecond line")
	if strings.Contains(got, "\n") {
		t.Errorf("title contains newline: %q", got)
	}
	if got != "first line" {
		t.Errorf("title = %q, want 'first line'", got)
	}
}

func TestChatStoreListOrderedByUpdatedDesc(t *testing.T) {
	s := newTestChatStore(t)
	ctx := context.Background()
	// Create three sessions, each with one message, in order.
	idA, _ := s.Create(ctx)
	_ = s.Append(ctx, idA, "user", "A")
	idB, _ := s.Create(ctx)
	_ = s.Append(ctx, idB, "user", "B")
	idC, _ := s.Create(ctx)
	_ = s.Append(ctx, idC, "user", "C")

	// Poke A to bump its updated_at — it should move to the front.
	_ = s.Append(ctx, idA, "user", "A again")

	list, err := s.List(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list len = %d, want 3", len(list))
	}
	if list[0].ID != idA {
		t.Errorf("first session = %s, want %s (most recently updated)", list[0].ID, idA)
	}
}

func TestChatStoreDeleteCascades(t *testing.T) {
	s := newTestChatStore(t)
	ctx := context.Background()
	id, _ := s.Create(ctx)
	_ = s.Append(ctx, id, "user", "hi")
	_ = s.Append(ctx, id, "assistant", "hello")

	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Messages should be gone too (FK cascade).
	msgs, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages after delete; got %d", len(msgs))
	}
	// And the session shouldn't appear in List.
	list, _ := s.List(ctx, 10)
	if len(list) != 0 {
		t.Errorf("expected 0 sessions after delete; got %d", len(list))
	}
}

func TestChatStoreRename(t *testing.T) {
	s := newTestChatStore(t)
	ctx := context.Background()
	id, _ := s.Create(ctx)
	_ = s.Append(ctx, id, "user", "original")

	if err := s.Rename(ctx, id, "a better title"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	list, _ := s.List(ctx, 10)
	if list[0].Title != "a better title" {
		t.Errorf("title = %q, want 'a better title'", list[0].Title)
	}
}

func TestChatStoreAppendRejectsBadSession(t *testing.T) {
	s := newTestChatStore(t)
	err := s.Append(context.Background(), "nonexistent", "user", "hi")
	if err == nil {
		t.Fatal("expected error appending to missing session")
	}
}

func TestChatStoreAppendRejectsBadRole(t *testing.T) {
	s := newTestChatStore(t)
	id, _ := s.Create(context.Background())
	err := s.Append(context.Background(), id, "bogus", "hi")
	if err == nil {
		t.Fatal("expected error for bad role")
	}
}
