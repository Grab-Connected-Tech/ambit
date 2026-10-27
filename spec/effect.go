package spec

import (
	"time"

	"github.com/Grab-Connected-Tech/ambit/model"
)

// Effect is a declared side effect (§5.8). Effects are deferred by default: the
// call is recorded in the outbox inside the action's transaction and runs after
// commit, which is what makes "sent email for a rolled-back change" impossible.
type Effect struct {
	typ      model.EffectKind
	mode     string
	hosts    []string
	topic    string
	bucket   string
	template string
	job      *ActionDecl
	comp     string
	missing  bool
	after    string
}

// Immediate switches the effect to run during the action, so its result can be
// used before commit. Immediate effects must be idempotent and always rank at
// least medium risk in the behavior diff (§10.2).
func (e *Effect) Immediate() *Effect {
	if e == nil {
		return nil
	}
	e.mode = "immediate"
	return e
}

// Template names a declared email or SMS template.
func (e *Effect) Template(name string) *Effect {
	if e != nil {
		e.template = name
	}
	return e
}

// Compensate names the effect the runtime enqueues if the transaction rolls back
// after an immediate effect succeeded (§10.2).
func (e *Effect) Compensate(name string) *Effect {
	if e != nil {
		e.comp = name
	}
	return e
}

// EmailEffect declares an outbound email. It is named EmailEffect rather than
// Email because s.Email is the field builder (§5.2), and one package holds both.
func EmailEffect(opts ...EffectOption) *Effect {
	e := &Effect{typ: model.EffectEmail, mode: "deferred"}
	for _, o := range opts {
		o.applyEffect(e)
	}
	return e
}

// EffectOption mutates an email effect.
type EffectOption interface{ applyEffect(*Effect) }

type effectOpt func(*Effect)

func (f effectOpt) applyEffect(e *Effect) { f(e) }

// Template names the declared template an email effect may send.
func Template(name string) EffectOption {
	return effectOpt(func(e *Effect) { e.template = name })
}

// SMSEffect declares a text-message effect.
func SMSEffect(opts ...EffectOption) *Effect {
	e := &Effect{typ: model.EffectSMS, mode: "deferred"}
	for _, o := range opts {
		o.applyEffect(e)
	}
	return e
}

// HTTP declares an outbound request. The host allowlist is enforced at runtime,
// after DNS, with private and link-local ranges blocked (§9.1).
func HTTP(opts ...HTTPOption) *Effect {
	e := &Effect{typ: model.EffectHTTP, mode: "deferred"}
	for _, o := range opts {
		o.applyEffect(e)
	}
	if len(e.hosts) == 0 {
		e.missing = true
	}
	return e
}

// HTTPOption mutates an HTTP effect.
type HTTPOption interface{ applyEffect(*Effect) }

// Hosts is the allowlist an HTTP effect may contact.
func Hosts(hosts ...string) HTTPOption {
	return effectOpt(func(e *Effect) { e.hosts = append(e.hosts, hosts...) })
}

// Webhook declares signed outbound webhooks to subscribers of a topic.
func Webhook(opts ...WebhookOption) *Effect {
	e := &Effect{typ: model.EffectWebhook, mode: "deferred"}
	for _, o := range opts {
		o.applyEffect(e)
	}
	if e.topic == "" {
		e.missing = true
	}
	return e
}

// WebhookOption mutates a webhook effect.
type WebhookOption interface{ applyEffect(*Effect) }

// Topic names the subscription channel for a webhook effect.
func Topic(name string) WebhookOption {
	return effectOpt(func(e *Effect) { e.topic = name })
}

// Storage declares object storage access through a configured provider.
func Storage(opts ...StorageOption) *Effect {
	e := &Effect{typ: model.EffectStorage, mode: "deferred"}
	for _, o := range opts {
		o.applyEffect(e)
	}
	if e.bucket == "" {
		e.missing = true
	}
	return e
}

// StorageOption mutates a storage effect.
type StorageOption interface{ applyEffect(*Effect) }

// Bucket names the storage bucket an effect may touch.
func Bucket(name string) StorageOption {
	return effectOpt(func(e *Effect) { e.bucket = name })
}

// Enqueue starts a job after the current transaction commits (§5.8). It is the
// replacement for a bare go statement (AM-PERF-003).
func Enqueue(job *ActionDecl) *Effect {
	e := &Effect{typ: model.EffectEnqueue, mode: "deferred", job: job}
	if job == nil {
		e.missing = true
	}
	return e
}

// After delays a queued job (§10.4).
func (e *Effect) After(d time.Duration) *Effect {
	if e != nil {
		e.after = d.String()
	}
	return e
}
