package runtime

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abhilashiig/ambit/am"
	"github.com/abhilashiig/ambit/model"
)

// The query cache of §11.3. An entry is keyed by the action, the tenant, the actor's
// role set and the input, so it never crosses an access boundary. It is valid for one
// generation: the sum of the per-tenant counters of the resources the action declared
// in InvalidatedBy. Every write bumps its resource's counter inside its own transaction,
// so invalidation is exactly as durable as the write and holds across instances; no
// entry is ever scanned or deleted. Entries live in process, and also in Postgres when
// cache.shared is set.

// queryCache is one call's view of the cache; a nil value caches nothing.
type queryCache struct {
	a      *App
	key    string
	tenant am.ID
	ttl    time.Duration
	gen    int64
}

func (a *App) cacheFor(ctx context.Context, h *Handler, actor am.Actor, in *Call) *queryCache {
	if h.Kind != model.KindQuery || h.Spec.Cache == nil || actor.Tenant.IsNil() {
		return nil
	}
	ttl, err := time.ParseDuration(h.Spec.Cache.TTL)
	if err != nil || ttl <= 0 {
		return nil
	}
	input := in.Body
	if len(input) == 0 && len(in.Query) > 0 {
		input = []byte(fmtQuery(in.Query))
	}
	c := &queryCache{a: a, key: cacheKey(h, actor, input), tenant: actor.Tenant, ttl: ttl}
	refs := make([]string, len(h.Spec.Cache.InvalidatedBy))
	for i, r := range h.Spec.Cache.InvalidatedBy {
		refs[i] = canonRef(r)
	}
	err = a.framework(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(sum(generation), 0) FROM am_cache_generation WHERE tenant_id = $1 AND resource = ANY($2)`,
			actor.Tenant.Bytes(), refs).Scan(&c.gen)
	})
	if err != nil {
		a.Log.Warn("cache generation unavailable, not caching", "err", err)
		return nil
	}
	return c
}

func (c *queryCache) lookup(ctx context.Context) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	if body, ok := c.a.Caches.get(c.key, c.ttl, c.gen); ok {
		c.a.metric("am_cache_hit", 1)
		return body, true
	}
	if c.a.Cfg.CacheShared {
		var body []byte
		err := c.a.framework(ctx, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT value::text FROM am_cache_entry WHERE cache_key = $1 AND generation = $2 AND expires_at > now()`,
				c.key, c.gen).Scan(&body)
		})
		if err == nil {
			c.a.Caches.put(c.key, body, c.ttl, c.gen)
			c.a.metric("am_cache_hit", 1)
			return body, true
		}
	}
	c.a.metric("am_cache_miss", 1)
	return nil, false
}

func (c *queryCache) store(ctx context.Context, body []byte) {
	if c == nil {
		return
	}
	c.a.Caches.put(c.key, body, c.ttl, c.gen)
	if !c.a.Cfg.CacheShared {
		return
	}
	err := c.a.framework(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO am_cache_entry (id, cache_key, tenant_id, value, expires_at, generation)
			VALUES ($1, $2, $3, $4::jsonb, now() + $5::interval, $6)
			ON CONFLICT (cache_key) DO UPDATE SET value = EXCLUDED.value, expires_at = EXCLUDED.expires_at, generation = EXCLUDED.generation`,
			am.NewID().Bytes(), c.key, c.tenant.Bytes(), string(body), seconds(c.ttl), c.gen)
		return err
	})
	if err != nil {
		c.a.Log.Warn("shared cache store", "err", err)
	}
}

// bumpGenerations advances the counter of every resource this transaction wrote, in the
// transaction, so a rolled-back write invalidates nothing.
func bumpGenerations(ctx context.Context, tx pgx.Tx, d *DB) error {
	seen := map[string]bool{}
	for _, r := range d.Audit {
		key := string(r.Tenant) + "|" + r.Resource
		if r.Tenant.IsNil() || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := tx.Exec(ctx, `INSERT INTO am_cache_generation (id, tenant_id, resource, generation) VALUES ($1, $2, $3, 1)
			ON CONFLICT (tenant_id, resource) DO UPDATE SET generation = am_cache_generation.generation + 1, updated_at = now()`,
			am.NewID().Bytes(), r.Tenant.Bytes(), canonRef(r.Resource)); err != nil {
			return translate(err)
		}
	}
	return nil
}

// canonRef spells an app-level resource the same way whichever side named it
// (".User" from a table, "app.User" from a spec).
func canonRef(ref string) string {
	if strings.HasPrefix(ref, ".") {
		return "app" + ref
	}
	return ref
}

func fmtQuery(q map[string][]string) string {
	var b strings.Builder
	for _, k := range sortedKeysOf(q) {
		b.WriteString(k + "=" + strings.Join(q[k], ",") + "&")
	}
	return b.String()
}

func sortedKeysOf(q map[string][]string) []string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
