package ks

import (
	"context"
	"sort"
	"time"
)

// ActorKind classifies who an action runs as.
type ActorKind string

const (
	ActorAnonymous ActorKind = "anonymous"
	ActorUser      ActorKind = "user"
	ActorService   ActorKind = "service"
	ActorSystem    ActorKind = "system"
)

// Actor is the resolved identity for a request. It is immutable once built: the
// runtime, not action code, decides it.
type Actor struct {
	Kind        ActorKind
	ID          ID
	Tenant      ID
	Roles       []string
	Authed      bool
	SessionID   ID
	TokenScopes []string
	// Snapshot marks an actor frozen at enqueue time (RunAs(Enqueuer)).
	Snapshot bool
}

// Anonymous is the actor used when credentials are missing or expired.
func Anonymous() Actor { return Actor{Kind: ActorAnonymous} }

// System builds the system actor for a tenant: every role, one tenant only.
func System(tenant ID) Actor {
	return Actor{Kind: ActorSystem, ID: tenant, Tenant: tenant, Authed: true, Roles: nil}
}

// HasRole reports membership of r. The system actor holds every role inside its
// own tenant but never crosses the tenant boundary (§5.7).
func (a Actor) HasRole(r string) bool {
	for _, got := range a.Roles {
		if got == r {
			return true
		}
	}
	return false
}

// SortedRoles returns the role set in canonical order, for cache keys and spans.
func (a Actor) SortedRoles() []string {
	out := append([]string(nil), a.Roles...)
	sort.Strings(out)
	return out
}

// IsAnonymous reports whether no credential was presented.
func (a Actor) IsAnonymous() bool { return a.Kind == ActorAnonymous || !a.Authed }

// CanScope reports whether the actor's API token (if any) allows an action.
func (a Actor) CanScope(action string) bool {
	if len(a.TokenScopes) == 0 {
		return true
	}
	for _, s := range a.TokenScopes {
		if s == action || s == "*" {
			return true
		}
	}
	return false
}

type actorKey struct{}

// WithActor stores an actor on a context.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the actor carried by a context, or the anonymous actor.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok {
		return a
	}
	return Anonymous()
}

// Clock is the only source of time available to action code (§7.4, KS-CODE-004).
type Clock interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

// SystemClock follows the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time                  { return time.Now().UTC() }
func (SystemClock) Since(t time.Time) time.Duration { return time.Since(t) }

// FakeClock is a controllable clock used by the test harness.
type FakeClock struct{ t time.Time }

// NewFakeClock starts a fake clock at t.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{t: t.UTC()} }

func (c *FakeClock) Now() time.Time { return c.t }

func (c *FakeClock) Since(t time.Time) time.Duration { return c.t.Sub(t) }

// Advance moves the fake clock forward.
func (c *FakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// FrozenClock is a Clock pinned to an instant; the default in generated tests.
func FrozenClock(t time.Time) *FakeClock { return NewFakeClock(t) }
