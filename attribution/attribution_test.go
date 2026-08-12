package attribution

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

type fakeRouter struct {
	mapping map[string]string
	err     error
}

func (f *fakeRouter) Lookup(ctx context.Context, messageID string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	appID, found := f.mapping[messageID]
	return appID, found, nil
}

type fakeRegistry struct {
	candidates map[string][]Candidate
	pins       map[string]string
	err        error

	setPinCalls     []struct{ waID, appID string }
	refreshPinCalls []string
}

func (f *fakeRegistry) Candidates(ctx context.Context, waID string) ([]Candidate, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.candidates[waID], nil
}

func (f *fakeRegistry) GetPin(ctx context.Context, waID string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	appID, found := f.pins[waID]
	return appID, found, nil
}

func (f *fakeRegistry) SetPin(ctx context.Context, waID, appID string, ttl time.Duration) error {
	f.setPinCalls = append(f.setPinCalls, struct{ waID, appID string }{waID, appID})
	if f.pins == nil {
		f.pins = map[string]string{}
	}
	f.pins[waID] = appID
	return nil
}

func (f *fakeRegistry) RefreshPin(ctx context.Context, waID string, ttl time.Duration) error {
	f.refreshPinCalls = append(f.refreshPinCalls, waID)
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(nilWriter{}, nil))
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestResolve_Exact(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{"wamid.123": "app-a"}}
	reg := &fakeRegistry{}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "wamid.123")

	if res.Level != LevelExact {
		t.Fatalf("expected level %q, got %q", LevelExact, res.Level)
	}
	if res.ResolvedAppID != "app-a" {
		t.Fatalf("expected resolved app_id %q, got %q", "app-a", res.ResolvedAppID)
	}
}

func TestResolve_UnknownWhenNoContextID(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "")

	if res.Level != LevelUnknown {
		t.Fatalf("expected level %q, got %q", LevelUnknown, res.Level)
	}
	if res.ResolvedAppID != "" {
		t.Fatalf("expected no resolved app_id, got %q", res.ResolvedAppID)
	}
}

func TestResolve_UnknownWhenContextIDMisses(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "wamid.does-not-exist")

	if res.Level != LevelUnknown {
		t.Fatalf("expected level %q, got %q", LevelUnknown, res.Level)
	}
}

func TestResolve_Inferred_ExactlyOneFlagged(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{candidates: map[string][]Candidate{
		"5511999999999": {
			{AppID: "app-a", MessageID: "wamid.a", ExpectsReply: true, SentAt: 200},
			{AppID: "app-b", MessageID: "wamid.b", ExpectsReply: false, SentAt: 100},
		},
	}}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "")

	if res.Level != LevelInferred {
		t.Fatalf("expected level %q, got %q", LevelInferred, res.Level)
	}
	if res.ResolvedAppID != "app-a" {
		t.Fatalf("expected resolved app_id %q, got %q", "app-a", res.ResolvedAppID)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("expected both candidates shipped, got %d", len(res.Candidates))
	}
}

func TestResolve_Ambiguous_TwoFlagged(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{candidates: map[string][]Candidate{
		"5511999999999": {
			{AppID: "app-a", MessageID: "wamid.a", ExpectsReply: true, SentAt: 200},
			{AppID: "app-c", MessageID: "wamid.c", ExpectsReply: true, SentAt: 100},
		},
	}}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "")

	if res.Level != LevelAmbiguous {
		t.Fatalf("expected level %q, got %q", LevelAmbiguous, res.Level)
	}
	if res.ResolvedAppID != "" {
		t.Fatalf("expected no resolved app_id, got %q", res.ResolvedAppID)
	}
	if len(res.Candidates) != 2 {
		t.Fatalf("expected both candidates shipped, got %d", len(res.Candidates))
	}
}

func TestResolve_Ambiguous_NoneFlagged(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{candidates: map[string][]Candidate{
		"5511999999999": {
			{AppID: "app-a", MessageID: "wamid.a", ExpectsReply: false, SentAt: 200},
			{AppID: "app-b", MessageID: "wamid.b", ExpectsReply: false, SentAt: 100},
		},
	}}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "")

	if res.Level != LevelAmbiguous {
		t.Fatalf("expected level %q, got %q", LevelAmbiguous, res.Level)
	}
}

func TestResolve_ExactTakesPrecedenceOverInferred(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{"wamid.123": "app-b"}}
	reg := &fakeRegistry{candidates: map[string][]Candidate{
		"5511999999999": {
			{AppID: "app-a", MessageID: "wamid.a", ExpectsReply: true, SentAt: 200},
		},
	}}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "wamid.123")

	if res.Level != LevelExact {
		t.Fatalf("expected exact to win over inferred, got %q", res.Level)
	}
	if res.ResolvedAppID != "app-b" {
		t.Fatalf("expected resolved app_id %q, got %q", "app-b", res.ResolvedAppID)
	}
}

func TestResolve_CandidateRegistryErrorDegradesToUnknown(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{err: fmt.Errorf("redis down")}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "")

	if res.Level != LevelUnknown {
		t.Fatalf("expected level %q, got %q", LevelUnknown, res.Level)
	}
}

func TestResolve_Pinned(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{pins: map[string]string{"5511999999999": "app-b"}}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "")

	if res.Level != LevelPinned {
		t.Fatalf("expected level %q, got %q", LevelPinned, res.Level)
	}
	if res.ResolvedAppID != "app-b" {
		t.Fatalf("expected resolved app_id %q, got %q", "app-b", res.ResolvedAppID)
	}
	if len(reg.refreshPinCalls) != 1 {
		t.Fatalf("expected pin TTL to be refreshed once, got %d calls", len(reg.refreshPinCalls))
	}
}

func TestResolve_PinTakesPrecedenceOverExact(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{"wamid.123": "app-a"}}
	reg := &fakeRegistry{pins: map[string]string{"5511999999999": "app-b"}}
	r := NewResolver(rtr, reg, discardLogger())

	res := r.Resolve(context.Background(), "5511999999999", "wamid.123")

	if res.Level != LevelPinned {
		t.Fatalf("expected pin to win over exact, got %q", res.Level)
	}
	if res.ResolvedAppID != "app-b" {
		t.Fatalf("expected resolved app_id %q, got %q", "app-b", res.ResolvedAppID)
	}
}

func TestResolve_ExactSetsPin(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{"wamid.123": "app-a"}}
	reg := &fakeRegistry{}
	r := NewResolver(rtr, reg, discardLogger())

	r.Resolve(context.Background(), "5511999999999", "wamid.123")

	if len(reg.setPinCalls) != 1 || reg.setPinCalls[0].appID != "app-a" {
		t.Fatalf("expected exact match to pin app-a, got %+v", reg.setPinCalls)
	}
}

func TestResolve_InferredSetsPin(t *testing.T) {
	rtr := &fakeRouter{mapping: map[string]string{}}
	reg := &fakeRegistry{candidates: map[string][]Candidate{
		"5511999999999": {
			{AppID: "app-a", MessageID: "wamid.a", ExpectsReply: true, SentAt: 200},
		},
	}}
	r := NewResolver(rtr, reg, discardLogger())

	r.Resolve(context.Background(), "5511999999999", "")

	if len(reg.setPinCalls) != 1 || reg.setPinCalls[0].appID != "app-a" {
		t.Fatalf("expected inferred match to pin app-a, got %+v", reg.setPinCalls)
	}
}

func TestBuildEnvelope_NilResolvedAppIDWhenUnknown(t *testing.T) {
	env := BuildEnvelope(Result{Level: LevelUnknown}, []byte(`{"foo":"bar"}`))

	if env.Version != 1 {
		t.Fatalf("expected version 1, got %d", env.Version)
	}
	if env.ResolvedAppID != nil {
		t.Fatalf("expected nil resolved_app_id, got %q", *env.ResolvedAppID)
	}
	if env.Candidates == nil {
		t.Fatal("expected candidates to be an empty slice, not nil")
	}
	if string(env.Payload) != `{"foo":"bar"}` {
		t.Fatalf("payload not carried byte-identical, got %q", env.Payload)
	}
}

func TestBuildEnvelope_ResolvedAppIDSet(t *testing.T) {
	env := BuildEnvelope(Result{Level: LevelExact, ResolvedAppID: "app-a"}, []byte(`{}`))

	if env.ResolvedAppID == nil || *env.ResolvedAppID != "app-a" {
		t.Fatalf("expected resolved_app_id %q, got %v", "app-a", env.ResolvedAppID)
	}
}
