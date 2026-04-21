package web

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

func TestRecentCallsRingCapacity(t *testing.T) {
	s := NewState()
	s.cap = 5 // force eviction quickly
	for i := 0; i < 10; i++ {
		s.Record(CallRecord{Provider: "p", Prompt: fmt.Sprintf("%d", i), CostUSD: 0.01})
	}
	rec := s.Recent(0)
	if len(rec) != 5 {
		t.Fatalf("recent len = %d; want 5", len(rec))
	}
	// Newest first.
	if rec[0].Prompt != "9" || rec[4].Prompt != "5" {
		t.Errorf("order off: %+v", rec)
	}
}

func TestPendingRecordCountedButUpdatable(t *testing.T) {
	s := NewState()
	id := s.Record(CallRecord{
		Timestamp: time.Now(),
		Task:      "generate",
		Prompt:    "show me the spend",
		Pending:   true,
	})
	if id == "" {
		t.Fatal("Record did not return an id for an un-id'd placeholder")
	}

	// Mid-stream snapshot: the spend tool sees the pending turn in
	// CallCount but not in totals (no provider/cost yet).
	if got := s.CallCount(); got != 1 {
		t.Errorf("CallCount = %d; want 1 (pending placeholder)", got)
	}
	if got := s.TotalSpend(); got != 0 {
		t.Errorf("TotalSpend = %v; want 0 for pending", got)
	}
	// Recent() hides pending rows so the Recent Calls table doesn't
	// render an empty-provider row.
	if r := s.Recent(10); len(r) != 0 {
		t.Errorf("Recent len = %d; want 0 (pending hidden)", len(r))
	}

	// Usage arrives: patch the record.
	ok := s.UpdateByID(id, func(r *CallRecord) {
		r.Provider = "anthropic"
		r.Model = "claude-sonnet-4-6"
		r.CostUSD = 0.001
		r.Pending = false
	})
	if !ok {
		t.Fatal("UpdateByID returned false on a still-live placeholder")
	}
	if got := s.TotalSpend(); got < 0.0009 || got > 0.0011 {
		t.Errorf("TotalSpend after update = %v; want ~0.001", got)
	}
	if r := s.Recent(10); len(r) != 1 {
		t.Errorf("Recent len after update = %d; want 1", len(r))
	}
}

func TestRemoveByIDDropsCancelledPlaceholder(t *testing.T) {
	s := NewState()
	id := s.Record(CallRecord{Pending: true, Task: "generate"})
	if got := s.CallCount(); got != 1 {
		t.Fatalf("CallCount after record = %d; want 1", got)
	}
	if !s.RemoveByID(id) {
		t.Fatal("RemoveByID returned false for a known id")
	}
	if got := s.CallCount(); got != 0 {
		t.Errorf("CallCount after remove = %d; want 0", got)
	}
}

func TestSpendAggregations(t *testing.T) {
	s := NewState()
	s.Record(CallRecord{Provider: "anthropic", Task: "reason", CostUSD: 0.10})
	s.Record(CallRecord{Provider: "anthropic", Task: "classify", CostUSD: 0.05})
	s.Record(CallRecord{Provider: "openai", Task: "reason", CostUSD: 0.20})

	if got := s.TotalSpend(); got < 0.34 || got > 0.36 {
		t.Errorf("total = %v; want ~0.35", got)
	}
	byProv := s.SpendByProvider()
	if len(byProv) != 2 {
		t.Fatalf("byProv len = %d", len(byProv))
	}
	if byProv[0].Provider != "anthropic" { // sort order
		t.Errorf("byProv[0] = %+v", byProv[0])
	}
	byTask := s.SpendByTask()
	if len(byTask) != 2 {
		t.Fatalf("byTask len = %d", len(byTask))
	}
}

func TestConcurrentRecord(t *testing.T) {
	s := NewState()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				s.Record(CallRecord{Provider: "p", CostUSD: 0.001})
			}
		}()
	}
	wg.Wait()
	got := s.TotalSpend()
	// 200 records × 0.001 = 0.2; with the default cap=100, sum
	// should be 0.100 (the 100 survivors). Allow fp slack.
	if got < 0.09 || got > 0.11 {
		t.Errorf("total = %v; want ~0.10", got)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := NewState()
	sess := &ChatSession{
		ID:        "abc",
		CreatedAt: time.Now(),
		Call:      hippo.Call{Prompt: "hi"},
	}
	s.PutSession("abc", sess)
	if got := s.TakeSession("abc"); got == nil || got.Call.Prompt != "hi" {
		t.Fatalf("TakeSession = %+v", got)
	}
	if got := s.TakeSession("abc"); got != nil {
		t.Fatalf("second Take = %+v; want nil", got)
	}
}

func TestPreviewTruncation(t *testing.T) {
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	s := NewState()
	s.Record(CallRecord{Prompt: string(long)})
	rec := s.Recent(1)
	if len(rec[0].Prompt) > 210 {
		t.Errorf("prompt not truncated: %d", len(rec[0].Prompt))
	}
}
