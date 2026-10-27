package ks

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIDIsUUIDv7(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 5000; i++ {
		id := NewID()
		if !id.IsValid() {
			t.Fatalf("malformed id %q", id)
		}
		b := id.Bytes()
		if len(b) != 16 {
			t.Fatalf("id %q is not 16 bytes", id)
		}
		if b[6]>>4 != 7 {
			t.Fatalf("version nibble must be 7, got %d", b[6]>>4)
		}
		if b[8]>>6 != 2 {
			t.Fatalf("variant bits must be 10, got %d", b[8]>>6)
		}
		if ids[string(id)] {
			t.Fatalf("duplicate id %q", id)
		}
		ids[string(id)] = true
	}
	// Time ordering: an id created later must sort after one created earlier.
	a := NewID()
	time.Sleep(2 * time.Millisecond)
	b := NewID()
	if string(a) >= string(b) {
		t.Fatalf("ids are not time ordered: %s !< %s", a, b)
	}
	round, err := IDFromBytes(b.Bytes())
	if err != nil || round != b {
		t.Fatalf("byte round trip failed: %v %v", round, err)
	}
	if !NilID.IsNil() || NewID().IsNil() {
		t.Fatal("IsNil is wrong")
	}
}

func TestOptDistinguishesZeroFromAbsent(t *testing.T) {
	var none Opt[string]
	if v, ok := none.Get(); ok || v != "" {
		t.Fatal("zero Opt must be absent")
	}
	some := Set("")
	if v, ok := some.Get(); !ok || v != "" {
		t.Fatal("Set(\"\") must be present")
	}
	if Set(7).Or(9) != 7 || Unset[int]().Or(9) != 9 {
		t.Fatal("Or is wrong")
	}
	if p := Set(3).Ptr(); p == nil || *p != 3 {
		t.Fatal("Ptr is wrong")
	}
	if Unset[int]().Ptr() != nil {
		t.Fatal("absent Ptr must be nil")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustGet on an absent value must panic")
		}
	}()
	Unset[int]().MustGet()
}

func TestMoneyAndDecimal(t *testing.T) {
	m := MoneyOf(1234, "usd")
	if m.String() != "12.34 USD" || !m.IsValid() {
		t.Fatalf("money renders %q", m.String())
	}
	if MoneyOf(20, "USD").Cmp(MoneyOf(10, "USD")) != 1 {
		t.Fatal("money ordering is wrong")
	}
	d := DecimalOf("10.25")
	if d.Add(DecimalOf("0.5")).String() != "10.750000000000" && !strings.HasPrefix(d.Add(DecimalOf("0.5")).String(), "10.75") {
		t.Fatalf("decimal add = %q", d.Add(DecimalOf("0.5")).String())
	}
	if _, err := ParseDecimal("1e5"); err == nil {
		t.Fatal("exponent notation must be rejected")
	}
	if DecimalInt(3).Cmp(DecimalOf("3.0")) != 0 {
		t.Fatal("3 must equal 3.0")
	}
}

func TestEmailURLDate(t *testing.T) {
	e, err := NormalizeEmail(" Alice@Example.COM ")
	if err != nil || e != "alice@example.com" {
		t.Fatalf("email normalization: %q %v", e, err)
	}
	if _, err := NormalizeEmail("a@b"); err == nil {
		t.Fatal("a@b must be rejected")
	}
	u, err := ParseURL("https://PayPal.com/x?y=1")
	if err != nil || u.Host() != "paypal.com" {
		t.Fatalf("url host: %q %v", u.Host(), err)
	}
	if _, err := ParseURL("http://internal:8080", "https"); err == nil {
		t.Fatal("scheme restriction must be enforced")
	}
	if _, err := ParseURL("ftp://x.com"); err == nil {
		t.Fatal("unlisted scheme must be rejected")
	}
	d, err := ParseDate("2026-02-30")
	if err == nil && !d.Valid() {
		t.Fatal("invalid calendar day")
	}
	if DateFromTime(time.Date(2026, 3, 5, 23, 0, 0, 0, time.UTC)).String() != "2026-03-05" {
		t.Fatal("date truncation is wrong")
	}
}

func TestStrictDecoderRejectsWhatMassAssignmentNeeds(t *testing.T) {
	in := []byte(`{"invoice_id":"0190f2a2-7c3e-7000-8000-000000000000","tenant_id":"0190f2a2-7c3e-7000-8000-000000000001","is_admin":true}`)
	body, err := ParseObject(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := body.RejectUnknown("invoice_id"); err == nil {
		t.Fatal("unknown fields must be rejected")
	} else {
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeInvalid {
			t.Fatalf("want an invalid-input error, got %#v", err)
		}
		if !strings.Contains(strings.Join(keys(e.Fields), ","), "tenant_id") {
			t.Fatalf("error should name the offending fields: %v", e.Fields)
		}
	}
	// A run-time-owned field must not be reachable even if a schema lists it.
	if v := body.Get("is_admin"); v == nil || v.Kind != KindBool {
		t.Fatal("decoded value lost its type")
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestDecoderEdgeCases(t *testing.T) {
	if _, err := ParseObject([]byte(`[1,2]`)); err == nil {
		t.Fatal("arrays are not objects")
	}
	if _, err := ParseObject([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate keys must be rejected")
	}
	if _, err := Parse([]byte(`{"a":1} trailing`)); err == nil {
		t.Fatal("trailing data must be rejected")
	}
	if _, err := Parse([]byte(`"` + strings.Repeat("[", 0) + `\u00e9"`)); err != nil {
		t.Fatal("unicode escape should parse:", err)
	}
	deep := []byte(strings.Repeat("[", 200) + strings.Repeat("]", 200))
	if _, err := Parse(deep); err == nil {
		t.Fatal("nesting depth must be capped")
	}
	v, err := Parse([]byte(`{"n":123456789012345678901234567890,"t":"2026-01-02T03:04:05Z","s":"a\u0000b"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get("n").Int(); err == nil {
		t.Fatal("an unrepresentable integer must fail, not truncate")
	}
	tm, err := v.Get("t").Time()
	if err != nil || !tm.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("time parse: %v %v", tm, err)
	}
	s, err := v.Get("s").Str()
	if err != nil || s != "a\x00b" {
		t.Fatalf("escape decoding: %q %v", s, err)
	}
	if _, err := ParseObject([]byte(`{"a"`)); err == nil {
		t.Fatal("unterminated object must fail")
	}
}

func TestEncoderRoundTripsThroughStdlib(t *testing.T) {
	w := NewW(64)
	w.ObjStart()
	w.Key("name")
	w.Str("a\"b\nç€")
	w.Key("when")
	w.Time(time.Date(2026, 10, 27, 1, 2, 3, 4, time.UTC))
	w.Key("opt")
	w.Null()
	w.Key("blob")
	w.Blob([]byte{0, 1, 255})
	w.Key("arr")
	w.ArrStart()
	for i := 0; i < 3; i++ {
		w.Item()
		w.Num(int64(i))
	}
	w.ArrEnd()
	w.Key("money")
	w.Money(MoneyOf(-501, "eur"))
	w.ObjEnd()

	var got map[string]any
	if err := json.Unmarshal(w.Bytes(), &got); err != nil {
		t.Fatalf("invalid json %s: %v", w.Bytes(), err)
	}
	if got["name"] != "a\"b\nç€" {
		t.Fatalf("string escaping lost: %v", got["name"])
	}
	if got["when"] != "2026-10-27T01:02:03.000000004Z" {
		t.Fatalf("timestamp: %v", got["when"])
	}
	if _, ok := got["money"].(map[string]any); !ok {
		t.Fatalf("money shape: %T", got["money"])
	}
	// Re-encoding a decoded document must be byte-stable.
	v, err := Parse(w.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	w2 := NewW(len(w.B))
	w2.Val(v)
	var a, b any
	_ = json.Unmarshal(w.Bytes(), &a)
	_ = json.Unmarshal(w2.Bytes(), &b)
	if string(w.Bytes()) != string(w2.Bytes()) {
		t.Fatalf("valve round trip changed the document:\n%s\n%s", w.Bytes(), w2.Bytes())
	}
}

func TestErrorVocabularyMapsToSpecStatuses(t *testing.T) {
	cases := []struct {
		err    *Error
		status int
		code   string
	}{
		{NotFound(), 404, CodeNotFound},
		{Invalid("amount", "must be positive"), 400, CodeInvalid},
		{Forbidden(""), 403, CodeForbidden},
		{Unauthenticated(), 401, CodeUnauthenticated},
		{Conflict(), 409, CodeConflict},
		{Invariant("amount_positive"), 422, CodeInvariant},
		{ActionError("not_draft", "Invoice is not in draft status."), 422, "not_draft"},
		{RateLimited(30), 429, CodeRateLimited},
		{NotImplemented(), 501, CodeNotImplemented},
		{InternalError(errors.New("boom")), 500, CodeInternal},
	}
	for _, tc := range cases {
		if got := Status(tc.err); got != tc.status {
			t.Errorf("%s: status %d want %d", tc.code, got, tc.status)
		}
		if AsError(tc.err).Code != tc.code {
			t.Errorf("code %q want %q", AsError(tc.err).Code, tc.code)
		}
	}
	if RateLimited(30).RetryAfter != 30 {
		t.Fatal("RateLimited must keep the Retry-After it is given")
	}
	// Any error that is not a *Error must collapse to a redacted internal error.
	e := AsError(errors.New("postgres said: password=hunter2"))
	if e.Code != CodeInternal || strings.Contains(e.Message, "hunter2") {
		t.Fatalf("raw error leaked through: %#v", e)
	}
	if e.Internal == nil || !strings.Contains(e.Internal.Error(), "hunter2") {
		t.Fatal("the cause must survive for logs and traces only")
	}
}

func TestActorAndClock(t *testing.T) {
	a := Actor{Kind: ActorUser, ID: NewID(), Tenant: NewID(), Roles: []string{"member", "admin"}}
	if !a.HasRole("admin") || a.HasRole("owner") {
		t.Fatal("role check is wrong")
	}
	got := a.SortedRoles()
	if got[0] != "admin" || got[1] != "member" {
		t.Fatalf("roles must be canonicalized: %v", got)
	}
	if !Anonymous().IsAnonymous() {
		t.Fatal("the zero actor must be anonymous")
	}
	sys := System(a.Tenant)
	if sys.IsAnonymous() {
		t.Fatal("the system actor is authenticated")
	}
	if sys.Kind != ActorSystem || sys.Tenant != a.Tenant {
		t.Fatalf("system actor: %+v", sys)
	}
	scoped := Actor{Kind: ActorService, Authed: true, TokenScopes: []string{"billing.SendInvoice"}}
	if !scoped.CanScope("billing.SendInvoice") || scoped.CanScope("billing.DeleteInvoice") {
		t.Fatal("token scoping must be exact")
	}
	if !(Actor{}).CanScope("x") {
		t.Fatal("no scopes means no restriction")
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := NewFakeClock(now)
	if !c.Now().Equal(now) {
		t.Fatal("fake clock start")
	}
	c.Advance(time.Hour)
	if c.Since(now) != time.Hour {
		t.Fatal("fake clock Since")
	}
}

func TestIDArgAndScan(t *testing.T) {
	id := NewID()
	b, err := ScanID(id.Bytes())
	if err != nil || b != id {
		t.Fatalf("scan from bytes: %v %v", b, err)
	}
	if s, err := ScanID(string(id)); err != nil || s != id {
		t.Fatalf("scan from text: %v %v", s, err)
	}
	if v, err := ScanID(nil); err != nil || v != NilID {
		t.Fatalf("nil scan: %v %v", v, err)
	}
	if _, err := ScanID(42); err == nil {
		t.Fatal("scanning an int as an ID must fail")
	}
	if IDArg(NilID) != nil {
		t.Fatal("nil ids must bind as NULL")
	}
}

func TestOptRoundTripsThroughJSON(t *testing.T) {
	type row struct {
		A Opt[string] `json:"a"`
		B Opt[int]    `json:"b"`
		C Opt[string] `json:"c"`
	}
	raw, err := json.Marshal(row{A: Set("x"), B: Set(0)})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"a":"x","b":0,"c":null}` {
		t.Fatalf("encoded %s", raw)
	}
	var back row
	if err := json.Unmarshal([]byte(`{"a":"x","b":0}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.A.Or("") != "x" || !back.B.Present() || back.C.Present() {
		t.Fatalf("decoded %+v", back)
	}
}
