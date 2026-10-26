package spec

import (
	"time"

	"keystone.dev/keystone/model"
)

// IdentityDecl collects the app's authentication surface (§9.2). Exactly one
// exists per app, declared in app/app.go.
type IdentityDecl struct {
	base model.Identity
}

// Identity declares how callers become actors.
func Identity(opts ...IdentityOption) *IdentityDecl {
	if registry.identity != nil {
		specErr("spec: s.Identity is declared more than once")
	}
	d := &IdentityDecl{base: model.Identity{
		Sessions: &model.Sessions{TTL: (14 * Day).String(), Rotate: true},
	}}
	for _, o := range opts {
		o.applyIdentity(d)
	}
	registry.identity = &d.base
	return d
}

// IdentityOption mutates the identity declaration.
type IdentityOption interface{ applyIdentity(*IdentityDecl) }

type identityOpt func(*IdentityDecl)

func (f identityOpt) applyIdentity(d *IdentityDecl) { f(d) }

// Sessions configures server-side cookie sessions: an opaque random token in an
// HttpOnly, Secure, SameSite=Lax cookie, rotated on login (§9.2).
func Sessions(opts ...SessionOption) IdentityOption {
	return identityOpt(func(d *IdentityDecl) {
		s := &model.Sessions{TTL: (14 * Day).String(), Rotate: true}
		for _, o := range opts {
			o.applySession(s)
		}
		d.base.Sessions = s
	})
}

// SessionOption mutates session settings.
type SessionOption interface{ applySession(*model.Sessions) }

type sessionOpt func(*model.Sessions)

func (f sessionOpt) applySession(s *model.Sessions) { f(s) }

// Expiry is a lifetime that can be declared anywhere a lifetime is accepted, so
// s.TTL reads the same in a session and in a cache declaration (§2.5: one way to do
// each thing, rather than two near-identical builders).
type Expiry struct{ d time.Duration }

func (e Expiry) applySession(s *model.Sessions) { s.TTL = e.d.String() }
func (e Expiry) applyCache(c *model.Cache)      { c.TTL = e.d.String() }

// String renders the canonical duration.
func (e Expiry) String() string { return e.d.String() }

// TTL sets how long something stays valid.
func TTL(d time.Duration) Expiry { return Expiry{d: d} }

// IdleTimeout logs an actor out after inactivity.
func IdleTimeout(d time.Duration) SessionOption {
	return sessionOpt(func(s *model.Sessions) { s.IdleTimeout = d.String() })
}

// NoRotate disables session rotation on privilege change. Rotation is on by
// default, and turning it off is a change the behavior diff reports.
func NoRotate() SessionOption {
	return sessionOpt(func(s *model.Sessions) { s.Rotate = false })
}

// APITokens enables hashed, optionally scoped bearer tokens (§9.2).
func APITokens(opts ...TokenOption) IdentityOption {
	return identityOpt(func(d *IdentityDecl) {
		t := &model.APITokens{}
		for _, o := range opts {
			o.applyTokens(t)
		}
		d.base.APITokens = t
	})
}

// TokenOption mutates API token settings.
type TokenOption interface{ applyTokens(*model.APITokens) }

type tokenOpt func(*model.APITokens)

func (f tokenOpt) applyTokens(t *model.APITokens) { f(t) }

// Scoped restricts each token to an explicit set of actions.
func Scoped() TokenOption {
	return tokenOpt(func(t *model.APITokens) { t.Scoped = true })
}

// OIDC enables authorization-code login with PKCE for one named provider.
func OIDC(provider string) IdentityOption {
	return identityOpt(func(d *IdentityDecl) {
		if provider == "" {
			specErr("spec: s.OIDC needs a provider name")
			return
		}
		d.base.OIDC = append(d.base.OIDC, model.OIDC{Provider: provider})
	})
}

// Passwords turns on the optional password module, off by default (§9.2).
func Passwords(opts ...PasswordOption) IdentityOption {
	return identityOpt(func(d *IdentityDecl) {
		p := &model.Passwords{Algorithm: "argon2id", BreachChecks: true}
		for _, o := range opts {
			o.applyPasswords(p)
		}
		d.base.Passwords = p
	})
}

// PasswordOption mutates password settings.
type PasswordOption interface{ applyPasswords(*model.Passwords) }

type passwordOpt func(*model.Passwords)

func (f passwordOpt) applyPasswords(p *model.Passwords) { f(p) }

// NoBreachChecks turns the breached-password check off.
func NoBreachChecks() PasswordOption {
	return passwordOpt(func(p *model.Passwords) { p.BreachChecks = false })
}

// AppName names the app for generated routes, clients and metrics. The CLI passes
// the name from ks.yaml when the spec does not declare one.
func AppName(name string) *AppNameDecl {
	if registry.appName != "" {
		specErr("spec: s.AppName declared twice")
	}
	if name == "" {
		specErr("spec: s.AppName cannot be empty")
		return nil
	}
	registry.appName = name
	return &AppNameDecl{name: name}
}

// AppNameDecl is the handle for an app name declaration.
type AppNameDecl struct{ name string }

// Name returns the declared app name.
func (a *AppNameDecl) Name() string { return a.name }

// Defaults overrides framework limits from spec rather than configuration.
func Defaults(opts ...DefaultOption) *DefaultsDecl {
	d := &DefaultsDecl{v: model.Defaults{}}
	for _, o := range opts {
		o.applyDefaults(&d.v)
	}
	registry.defaults = &d.v
	return d
}

// DefaultOption mutates app-wide defaults.
type DefaultOption interface{ applyDefaults(*model.Defaults) }

type defaultOpt func(*model.Defaults)

func (f defaultOpt) applyDefaults(d *model.Defaults) { f(d) }

// DefaultQueryBudget sets the per-action query ceiling (§8.4).
func DefaultQueryBudget(n int) DefaultOption {
	return defaultOpt(func(d *model.Defaults) {
		if n > 0 {
			d.QueryBudget = n
		}
	})
}

// DefaultMaxPageSize caps every paginated query.
func DefaultMaxPageSize(n int) DefaultOption {
	return defaultOpt(func(d *model.Defaults) { d.MaxPageSize = n })
}

// DefaultBodyLimit caps request bodies (§9.3).
func DefaultBodyLimit(n int64) DefaultOption {
	return defaultOpt(func(d *model.Defaults) { d.BodyLimitBytes = n })
}

// DefaultsDecl is the handle for a defaults declaration.
type DefaultsDecl struct{ v model.Defaults }
