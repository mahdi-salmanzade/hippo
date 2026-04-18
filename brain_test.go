package hippo

import (
	"context"
	"errors"
	"testing"
)

// fakeProvider is a minimal hippo.Provider for brain-level unit tests.
// Real provider tests live in the provider subpackages.
type fakeProvider struct {
	name    string
	privacy PrivacyTier
	resp    *Response
	err     error
	calls   int
}

func (f *fakeProvider) Name() string              { return f.name }
func (f *fakeProvider) Models() []ModelInfo       { return nil }
func (f *fakeProvider) Privacy() PrivacyTier      { return f.privacy }
func (f *fakeProvider) EstimateCost(Call) (float64, error) {
	return 0, nil
}
func (f *fakeProvider) Call(ctx context.Context, c Call) (*Response, error) {
	f.calls++
	return f.resp, f.err
}
func (f *fakeProvider) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	return nil, ErrNotImplemented
}

func TestBrainCallNoProvider(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Call(context.Background(), Call{Prompt: "hi"})
	if !errors.Is(err, ErrNoProviderAvailable) {
		t.Errorf("err = %v, want ErrNoProviderAvailable", err)
	}
}

func TestBrainCallDelegates(t *testing.T) {
	want := &Response{Text: "ok", Provider: "fake"}
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: want}
	b, _ := New(WithProvider(fp))

	got, err := b.Call(context.Background(), Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != want {
		t.Errorf("Call returned %v, want %v", got, want)
	}
	if fp.calls != 1 {
		t.Errorf("provider.Call count = %d, want 1", fp.calls)
	}
}

func TestBrainCallPrivacyViolation(t *testing.T) {
	fp := &fakeProvider{name: "cloud", privacy: PrivacyCloudOK, resp: &Response{}}
	b, _ := New(WithProvider(fp))

	_, err := b.Call(context.Background(), Call{Prompt: "hi", Privacy: PrivacyLocalOnly})
	if !errors.Is(err, ErrPrivacyViolation) {
		t.Errorf("err = %v, want ErrPrivacyViolation", err)
	}
	if fp.calls != 0 {
		t.Errorf("provider.Call called %d times despite privacy mismatch, want 0", fp.calls)
	}
}

func TestBrainStreamReturnsNotImplemented(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK}
	b, _ := New(WithProvider(fp))
	ch, err := b.Stream(context.Background(), Call{Prompt: "hi"})
	if ch != nil {
		t.Error("Stream returned non-nil channel, want nil")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}
