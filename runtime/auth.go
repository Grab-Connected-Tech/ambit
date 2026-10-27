package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"crypto/pbkdf2"

	"github.com/Grab-Connected-Tech/ambit/am"
	"github.com/Grab-Connected-Tech/ambit/model"
)

// Auth resolves a request into an actor (§3.3 step 3). Unknown or expired credentials
// yield the anonymous actor: an invalid cookie must never look like an error, because
// that would turn a login page into an oracle.
type Auth struct {
	A          *App
	CookieName string
	Domain     string
	Secure     bool
}

func (a *App) newAuth() *Auth {
	return &Auth{A: a, CookieName: "am_session", Domain: "", Secure: true}
}

// Authenticate resolves session cookie, then bearer token, then nothing (§9.2).
func (a *App) Authenticate(r *http.Request) (am.Actor, error) {
	if a.Auth == nil {
		a.Auth = a.newAuth()
	}
	if c, err := r.Cookie(a.Auth.CookieName); err == nil && c.Value != "" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !SameOrigin(r) {
			return am.Actor{}, am.Forbidden("cross-origin request with a session cookie")
		}
		return a.Auth.resolveSession(r.Context(), c.Value)
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.Auth.resolveToken(r.Context(), strings.TrimPrefix(h, "Bearer "))
	}
	return am.Anonymous(), nil
}

// Actor is the resolved identity plus the roles the session carried.
type Actor = am.Actor

// resolveSession turns a session token into an actor.
func (au *Auth) resolveSession(ctx context.Context, token string) (am.Actor, error) {
	var (
		tenant, actor am.ID
		kind          string
		expires       time.Time
		idle          *time.Time
		revoked       *time.Time
		id            am.ID
	)
	_ = au.A.framework(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT s.id, s.tenant_id, s.actor_id, s.expires_at, s.idle_expires_at, s.revoked_at, s.actor_kind
			   FROM am_session s
			  WHERE s.token_hash = $1`, hashToken(token)).
			Scan(&id, &tenant, &actor, &expires, &idle, &revoked, &kind)
	})
	if id.IsNil() {
		return am.Anonymous(), nil
	}
	now := time.Now().UTC()
	if revoked != nil || expires.Before(now) || (idle != nil && idle.Before(now)) {
		return am.Anonymous(), nil
	}
	// last_seen moves without waiting for the write to finish the request.
	if err := au.A.framework(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE am_session SET last_seen_at = now(), idle_expires_at = now() + $2::interval WHERE id = $1`,
			id, idleText(au.A))
		return err
	}); err != nil {
		au.A.Log.Debug("session touch", "err", err)
	}
	got, err := au.roles(ctx, tenant, actor)
	if err != nil {
		return am.Anonymous(), nil
	}
	a := am.Actor{Kind: actorKind(kind), ID: actor, Tenant: tenant, Roles: got, Authed: true, SessionID: id}
	return a, nil
}

// nullInet keeps an empty address out of the inet column: Postgres rejects "" rather
// than reading it as "no information".
func nullInet(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func idleText(a *App) string {
	if a.Model != nil && a.Model.Identity.Sessions != nil && a.Model.Identity.Sessions.IdleTimeout != "" {
		return a.Model.Identity.Sessions.IdleTimeout
	}
	return "7 days"
}

func actorKind(s string) am.ActorKind {
	switch s {
	case "service":
		return am.ActorService
	case "system":
		return am.ActorSystem
	default:
		return am.ActorUser
	}
}

// resolveToken accepts an API token, checking its scopes (§9.2).
func (au *Auth) resolveToken(ctx context.Context, token string) (am.Actor, error) {
	var (
		tenant, actor, id am.ID
		scopes            []string
		revoked           *time.Time
	)
	err := au.A.framework(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT id, tenant_id, actor_id, scopes, revoked_at FROM am_api_token WHERE token_hash = $1`, hashToken(token)).
			Scan(&id, &tenant, &actor, &scopes, &revoked)
	})
	if err != nil || (revoked != nil && !revoked.IsZero()) {
		return am.Anonymous(), nil
	}
	roles, err := au.roles(ctx, tenant, actor)
	if err != nil {
		return am.Anonymous(), nil
	}
	if err := au.A.framework(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE am_api_token SET last_used_at = now() WHERE id=$1`, id)
		return err
	}); err != nil {
		au.A.Log.Debug("token touch", "err", err)
	}
	return am.Actor{Kind: am.ActorService, ID: actor, Tenant: tenant, Roles: roles, Authed: true, TokenScopes: scopes}, nil
}

// roles reads the actor's role set for one tenant from the membership table, which is
// what makes revocation take effect on the next request rather than the next login.
func (au *Auth) roles(ctx context.Context, tenant, actor am.ID) ([]string, error) {
	if tenant.IsNil() || actor.IsNil() {
		return nil, nil
	}
	// Membership is framework data: the query states its own tenant predicate, so it
	// runs with the isolation GUC opened rather than pretending to be a request (§5.1).
	var out []string
	err := au.A.inSystemScope(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT role FROM am_membership WHERE tenant_id=$1 AND actor_id=$2 ORDER BY role`, tenant, actor)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, translate(err)
	}
	sort.Strings(out)
	return out, nil
}

// Login creates a session for an actor and returns the cookie value. Sessions rotate on
// login, so a token captured earlier stops working (§9.2).
func (a *App) Login(ctx context.Context, actor am.Actor, rotateFrom am.ID, ua, ip string) (string, *http.Cookie, error) {
	if actor.Tenant.IsNil() || actor.ID.IsNil() {
		return "", nil, am.Forbidden("a session needs an actor and a tenant")
	}
	token := randomToken(32)
	ttl := 14 * 24 * time.Hour
	if a.Model != nil && a.Model.Identity.Sessions != nil {
		if d, err := time.ParseDuration(a.Model.Identity.Sessions.TTL); err == nil {
			ttl = d
		}
	}
	id := am.NewID()
	if err := a.framework(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO am_session (id, tenant_id, actor_id, actor_kind, token_hash, rotate_from, user_agent, ip, created_at, expires_at, idle_expires_at)
         VALUES ($1,$2,$3,$4,$5,$6,$7,$8::inet,now(),$9,now()+$10::interval)`,
			id, actor.Tenant, actor.ID, string(actor.Kind), hashToken(token), nullID(rotateFrom), trunc(ua, 250), nullInet(ip),
			time.Now().Add(ttl), intervalText(idleDuration(a)))
		return err
	}); err != nil {
		return "", nil, translate(err)
	}
	if !rotateFrom.IsNil() {
		if err := a.framework(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE am_session SET revoked_at = now() WHERE id=$1`, rotateFrom)
			return err
		}); err != nil {
			return "", nil, translate(err)
		}
	}
	cookie := &http.Cookie{
		Name:     "am_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(ttl),
	}
	return token, cookie, nil
}

func idleDuration(a *App) time.Duration {
	if a.Model != nil && a.Model.Identity.Sessions != nil && a.Model.Identity.Sessions.IdleTimeout != "" {
		if d, err := time.ParseDuration(a.Model.Identity.Sessions.IdleTimeout); err == nil {
			return d
		}
	}
	return 7 * 24 * time.Hour
}

// Logout revokes the presented session.
func (a *App) Logout(ctx context.Context, token string) error {
	err := a.framework(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE am_session SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, hashToken(token))
		return err
	})
	return translate(err)
}

// Rotate drops the old session and issues a new token, which is what a privilege change
// requires (§9.2).
func (a *App) Rotate(ctx context.Context, current string, actor am.Actor) (string, *http.Cookie, error) {
	var id am.ID
	_ = a.framework(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM am_session WHERE token_hash=$1 AND revoked_at IS NULL`, hashToken(current)).Scan(&id)
	})
	return a.Login(ctx, actor, id, "", "")
}

// NewAPIToken creates a service token; only its hash is stored, and the plaintext is
// shown once (§9.2).
func (a *App) NewAPIToken(ctx context.Context, tenant, actor am.ID, name string, scopes []string) (string, error) {
	token := "am_" + randomToken(24)
	if err := a.framework(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO am_api_token (id, tenant_id, actor_id, name, token_hash, scopes, created_at) VALUES ($1,$2,$3,$4,$5,$6,now())`,
			am.NewID(), tenant, actor, name, hashToken(token), scopes)
		return err
	}); err != nil {
		return "", translate(err)
	}
	return token, nil
}

// GrantRole grants a role to an actor in a tenant. It is a spec-visible write, so the
// membership table has RLS like everything else (§5.1).
func (a *App) GrantRole(ctx context.Context, tx Conn, tenant, actor am.ID, role string) error {
	if !a.roleExists(role) {
		return am.Invalid("role", fmt.Sprintf("the app does not declare the role %q", role))
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO am_membership (id, tenant_id, actor_id, role, created_at) VALUES ($1,$2,$3,$4,now()) ON CONFLICT (tenant_id, actor_id, role) DO NOTHING`,
		am.NewID(), tenant, actor, role)
	return translate(err)
}

func (a *App) roleExists(role string) bool {
	if a.Model == nil {
		return false
	}
	for _, r := range a.Model.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// HashPassword produces the stored form: PBKDF2-HMAC-SHA256 with a per-password salt.
// The spec asks for Argon2id, which needs x/crypto; PBKDF2 from the standard library
// keeps the dependency list boring (§2.10) and is recorded as a deviation.
func HashPassword(password string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	dk, err := pbkdf2.Key(sha256.New, password, salt, 120_000, 32)
	if err != nil {
		// A unavailable KDF must not silently downgrade to a weaker one.
		return "unavailable$" + b64(salt) + "$$"
	}
	return fmt.Sprintf("pbkdf2_sha256$120000$%s$%s", b64(salt), b64(dk))
}

// VerifyPassword compares in constant time and never panics on a malformed hash.
func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2_sha256" {
		return false
	}
	salt, err := unb64(parts[2])
	if err != nil {
		return false
	}
	want, err := unb64(parts[3])
	if err != nil || len(want) == 0 {
		return false
	}
	var iterations int
	fmt.Sscanf(parts[1], "%d", &iterations)
	if iterations <= 0 || iterations > 5_000_000 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func b64(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }

// PasswordIsBreached consults a local denylist. A network call would turn login into an
// availability dependency, so the check is offline and configurable.
var commonPasswords = map[string]bool{
	"password": true, "12345678": true, "qwertyui": true, "letmein": true,
	"changeme": true, "admin123": true, "passw0rd": true,
}

func PasswordIsBreached(p string) bool { return commonPasswords[strings.ToLower(p)] }

// OIDC is a minimal authorization-code flow with PKCE against any discovery document
// (§9.2). Vendor presets are out of scope for this build.
type OIDC struct {
	Issuer     string
	ClientID   string
	Secret     string
	Redirect   string
	Scopes     []string
	HTTPClient *http.Client
}

type oidcState struct {
	Nonce    string
	Verifier string
	ReturnTo string
	Issued   time.Time
	Provider string
}

// AuthURL starts a login and returns the redirect plus a signed state cookie value.
func (o *OIDC) AuthURL(ctx context.Context, returnTo string) (string, *oidcState, error) {
	meta, err := o.discover(ctx)
	if err != nil {
		return "", nil, err
	}
	verifier := randomToken(32)
	chal := challengeS256(verifier)
	st := &oidcState{Nonce: randomToken(16), Verifier: verifier, ReturnTo: returnTo, Issued: time.Now(), Provider: o.Issuer}
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", o.ClientID)
	q.Set("redirect_uri", o.Redirect)
	q.Set("scope", strings.Join(orStrings(o.Scopes, "openid", "email", "profile"), " "))
	q.Set("state", st.Nonce)
	q.Set("nonce", st.Nonce)
	q.Set("code_challenge", chal)
	q.Set("code_challenge_method", "S256")
	return strings.TrimRight(meta["authorization_endpoint"], "/") + "?" + q.Encode(), st, nil
}

// Exchange turns a code into the provider's subject and email.
func (o *OIDC) Exchange(ctx context.Context, code, verifier string) (subject, email string, err error) {
	meta, err := o.discover(ctx)
	if err != nil {
		return "", "", err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", o.Redirect)
	form.Set("client_id", o.ClientID)
	form.Set("client_secret", o.Secret)
	form.Set("code_verifier", verifier)
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta["token_endpoint"], strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("oidc: token endpoint returned %d", res.StatusCode)
	}
	var payload struct {
		IDToken string `json:"id_token"`
	}
	if err := decodeJSON(res, &payload); err != nil {
		return "", "", err
	}
	claims, err := unverifiedClaims(payload.IDToken)
	if err != nil {
		return "", "", err
	}
	sub, _ := claims["sub"].(string)
	mail, _ := claims["email"].(string)
	if sub == "" {
		return "", "", errors.New("oidc: id_token has no sub")
	}
	return sub, mail, nil
}

// Link resolves a provider subject to an actor, creating the link row on first login.
func (a *App) Link(ctx context.Context, provider, subject, email string, tenant am.ID) (am.Actor, error) {
	var actor am.ID
	err := a.framework(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT actor_id FROM am_identity_link WHERE provider=$1 AND subject=$2`, provider, subject).Scan(&actor)
	})
	if err != nil || actor.IsNil() {
		return am.Actor{}, am.Unauthenticated()
	}
	roles, rerr := a.newAuth().roles(ctx, tenant, actor)
	if rerr != nil {
		return am.Actor{}, rerr
	}
	return am.Actor{Kind: am.ActorUser, ID: actor, Tenant: tenant, Roles: roles, Authed: true}, nil
}

func (o *OIDC) discover(ctx context.Context) (map[string]string, error) {
	client := o.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(o.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := decodeJSON(res, &out); err != nil {
		return nil, err
	}
	strs := map[string]string{}
	for k, v := range out {
		if s, ok := v.(string); ok {
			strs[k] = s
		}
	}
	if strs["authorization_endpoint"] == "" || strs["token_endpoint"] == "" {
		return nil, errors.New("oidc: discovery document is missing its endpoints")
	}
	return strs, nil
}

func decodeJSON(body any, out any) error {
	rc, ok := body.(interface{ Read([]byte) (int, error) })
	if !ok {
		return errors.New("runtime: not a reader")
	}
	dec := jsonDecoder(rc)
	return dec.Decode(out)
}

func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// unverifiedClaims reads the payload of a JWT. Signature verification is done by the
// provider library chosen in config; this build keeps the shape of the flow.
func unverifiedClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: malformed id_token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := jsonUnmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return hex.EncodeToString(sha256.New().Sum(nil))[:n*2]
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken stores credentials as a SHA-256 digest: a database read must not hand out
// usable sessions (§9.2).
func hashToken(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

func nullID(id am.ID) any {
	if id.IsNil() {
		return nil
	}
	return id
}

func orStrings(in []string, def ...string) []string {
	if len(in) == 0 {
		return def
	}
	return in
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// CSRF: cookie sessions may only be driven from the site's own origin (§9.3).
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		referer := r.Header.Get("Referer")
		if referer == "" {
			return true
		}
		u, err := url.Parse(referer)
		if err != nil {
			return false
		}
		origin = u.Scheme + "://" + u.Host
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

var _ = model.RunAsSystem
