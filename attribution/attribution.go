// Package attribution resolves which app's outbound message an inbound
// WhatsApp reply relates to. It produces evidence-backed attribution, never
// a guess presented as fact — ambiguity is surfaced to the receiver rather
// than silently resolved.
package attribution

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// pinTTL is how long a resolved attribution pins a conversation to its app,
// and how far rung 1 refreshes an existing pin on every match.
const pinTTL = 15 * time.Minute

const (
	LevelExact     = "exact"
	LevelPinned    = "pinned"
	LevelInferred  = "inferred"
	LevelAmbiguous = "ambiguous"
	LevelUnknown   = "unknown"
)

// Candidate is one outbound message that may be the target of an inbound reply.
type Candidate struct {
	AppID        string `json:"app_id"`
	MessageID    string `json:"message_id"`
	ExpectsReply bool   `json:"expects_reply"`
	Topic        string `json:"topic"`
	Snippet      string `json:"snippet"`
	SentAt       int64  `json:"sent_at"`
}

// Envelope is the payload delivered to message_receiver when enrichment is enabled.
type Envelope struct {
	Version       int             `json:"version"`
	Attribution   string          `json:"attribution"`
	ResolvedAppID *string         `json:"resolved_app_id"`
	Candidates    []Candidate     `json:"candidates"`
	Payload       json.RawMessage `json:"payload"`
}

// Result is the outcome of running the attribution ladder.
type Result struct {
	Level         string
	ResolvedAppID string // empty when no app could be resolved
	Candidates    []Candidate
}

// routerLookup is the subset of router.Router used by the attribution ladder.
// Declared as an interface so the ladder can be unit tested without Redis.
type routerLookup interface {
	Lookup(ctx context.Context, messageID string) (string, bool, error)
}

// store is the subset of registry.Registry used by the attribution ladder:
// candidate assembly and session pinning. Declared as an interface so the
// ladder can be unit tested without Redis.
type store interface {
	Candidates(ctx context.Context, waID string) ([]Candidate, error)
	GetPin(ctx context.Context, waID string) (string, bool, error)
	SetPin(ctx context.Context, waID, appID string, ttl time.Duration) error
	RefreshPin(ctx context.Context, waID string, ttl time.Duration) error
}

// Resolver runs the inbound attribution ladder.
type Resolver struct {
	router   routerLookup
	registry store
	log      *slog.Logger
}

func NewResolver(rtr routerLookup, reg store, log *slog.Logger) *Resolver {
	return &Resolver{router: rtr, registry: reg, log: log}
}

// Resolve runs the attribution ladder for a single inbound message.
// waID and contextID may be empty when the inbound payload doesn't carry them.
//
// Candidates are always assembled first (rungs 3-5 need them, and rungs 1-2
// ship them too so the receiver can sanity-check a resolved attribution).
// A Redis error degrades to an empty candidate list rather than failing the
// inbound message.
func (r *Resolver) Resolve(ctx context.Context, waID, contextID string) Result {
	candidates, err := r.registry.Candidates(ctx, waID)
	if err != nil {
		r.log.Error("attribution: candidate assembly error", "wa_id", waID, "err", err)
		candidates = nil
	}

	// Rung 1: pinned — a prior resolution (or outbound send) already pinned
	// this conversation to an app.
	if pinnedApp, found, err := r.registry.GetPin(ctx, waID); err != nil {
		r.log.Error("attribution: get pin error", "wa_id", waID, "err", err)
	} else if found {
		if err := r.registry.RefreshPin(ctx, waID, pinTTL); err != nil {
			r.log.Error("attribution: refresh pin error", "wa_id", waID, "err", err)
		}
		return Result{Level: LevelPinned, ResolvedAppID: pinnedApp, Candidates: candidates}
	}

	// Rung 2: exact — Meta sets context.id on quote-replies and interactive replies.
	if contextID != "" {
		appID, found, err := r.router.Lookup(ctx, contextID)
		if err != nil {
			r.log.Error("attribution: router lookup error", "context_id", contextID, "err", err)
		} else if found {
			if err := r.registry.SetPin(ctx, waID, appID, pinTTL); err != nil {
				r.log.Error("attribution: set pin error", "wa_id", waID, "err", err)
			}
			return Result{Level: LevelExact, ResolvedAppID: appID, Candidates: candidates}
		}
	}

	if len(candidates) == 0 {
		return Result{Level: LevelUnknown}
	}

	// Rung 3: inferred — exactly one candidate declared it expects a reply.
	var flagged []Candidate
	for _, c := range candidates {
		if c.ExpectsReply {
			flagged = append(flagged, c)
		}
	}
	if len(flagged) == 1 {
		if err := r.registry.SetPin(ctx, waID, flagged[0].AppID, pinTTL); err != nil {
			r.log.Error("attribution: set pin error", "wa_id", waID, "err", err)
		}
		return Result{Level: LevelInferred, ResolvedAppID: flagged[0].AppID, Candidates: candidates}
	}

	// Rung 4: ambiguous — candidates exist but nothing above resolved it.
	return Result{Level: LevelAmbiguous, Candidates: candidates}
}

// BuildEnvelope wraps an attribution result and the raw Meta payload for delivery.
func BuildEnvelope(res Result, payload json.RawMessage) Envelope {
	var resolved *string
	if res.ResolvedAppID != "" {
		id := res.ResolvedAppID
		resolved = &id
	}

	candidates := res.Candidates
	if candidates == nil {
		candidates = []Candidate{}
	}

	return Envelope{
		Version:       1,
		Attribution:   res.Level,
		ResolvedAppID: resolved,
		Candidates:    candidates,
		Payload:       payload,
	}
}
