package ks

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------- encoder (append based, no reflection) ----------

// W is a streaming JSON writer used by generated encoders.
type W struct {
	B    []byte
	deep int
	err  error
}

// Err is the first error an Any fallback met.
func (w *W) Err() error { return w.err }

// appender is a value that writes itself, such as Opt.
type appender interface{ AppendTo(*W) }

// Any writes a value of a framework type directly, and anything else through
// encoding/json. Generated encoders call it for every field (§11.1).
func (w *W) Any(v any) {
	switch t := v.(type) {
	case nil:
		w.Null()
	case string:
		w.Str(t)
	case ID:
		w.ID(t)
	case bool:
		w.Bool(t)
	case int:
		w.Num(int64(t))
	case int32:
		w.Num(int64(t))
	case int64:
		w.Num(t)
	case Version:
		w.Num(int64(t))
	case time.Time:
		w.Time(t)
	case Money:
		w.Money(t)
	case Decimal:
		w.Str(t.String())
	case appender:
		t.AppendTo(w)
	case json.Marshaler: // a loaded relation: its own generated encoder
		raw, err := t.MarshalJSON()
		if err != nil && w.err == nil {
			w.err = err
		}
		w.B = append(w.B, raw...)
	default:
		// A generated enum is a named string type.
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.String {
			w.Str(rv.String())
			return
		}
		w.fallback(v)
	}
}

func (w *W) fallback(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		if w.err == nil {
			w.err = err
		}
		w.Null()
		return
	}
	w.B = append(w.B, raw...)
}

// NewW returns a writer with a preallocated buffer.
func NewW(size int) *W {
	if size <= 0 {
		size = 256
	}
	return &W{B: make([]byte, 0, size)}
}

// Bytes returns the encoded document.
func (w *W) Bytes() []byte { return w.B }

// ObjStart opens an object.
func (w *W) ObjStart() { w.B = append(w.B, '{'); w.deep++ }

// ObjEnd closes an object.
func (w *W) ObjEnd() { w.B = append(w.B, '}'); w.deep-- }

// Key writes an object key.
func (w *W) Key(k string) {
	if n := len(w.B); n > 0 && w.B[n-1] != '{' {
		w.B = append(w.B, ',')
	}
	w.B = AppendJSONString(w.B, k)
	w.B = append(w.B, ':')
}

// Val writes any decoded value back out.
func (w *W) Val(v *Val) {
	if v == nil {
		w.B = append(w.B, "null"...)
		return
	}
	switch v.Kind {
	case KindNull:
		w.B = append(w.B, "null"...)
	case KindBool:
		if v.B {
			w.B = append(w.B, "true"...)
		} else {
			w.B = append(w.B, "false"...)
		}
	case KindNum:
		w.B = append(w.B, v.S...)
	case KindStr:
		w.B = AppendJSONString(w.B, v.S)
	case KindArr:
		w.ArrStart()
		for _, item := range v.Arr {
			w.Item()
			w.Val(item)
		}
		w.ArrEnd()
	case KindObj:
		w.ObjStart()
		for _, k := range v.Keys() {
			w.Key(k)
			w.Val(v.Get(k))
		}
		w.ObjEnd()
	}
}

// Str writes a string value.
func (w *W) Str(s string) { w.B = AppendJSONString(w.B, s) }

// Name writes a string value at a position that already knows its type.
func (w *W) Num(n int64) { w.B = strconv.AppendInt(w.B, n, 10) }

// Float writes a float64, rejecting non-finite values.
func (w *W) Float(f float64) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		w.B = append(w.B, "null"...)
		return
	}
	w.B = strconv.AppendFloat(w.B, f, 'f', -1, 64)
}

// Rat writes an exact decimal as a JSON number.
func (w *W) Rat(d Decimal) { w.B = append(w.B, d.String()...) }

// Bool writes a boolean.
func (w *W) Bool(b bool) {
	if b {
		w.B = append(w.B, "true"...)
	} else {
		w.B = append(w.B, "false"...)
	}
}

// Null writes null.
func (w *W) Null() { w.B = append(w.B, "null"...) }

// Time writes an RFC 3339 nanosecond timestamp.
func (w *W) Time(t time.Time) { w.B = AppendJSONString(w.B, t.UTC().Format(time.RFC3339Nano)) }

// OptTime writes a timestamp or null when absent.
func (w *W) OptTime(o Opt[time.Time]) {
	if v, ok := o.Get(); ok {
		w.Time(v)
		return
	}
	w.Null()
}

// ID writes an identifier.
func (w *W) ID(id ID) { w.Str(string(id)) }

// Money writes the "12.34 USD" minor-unit pair as an object.
func (w *W) Money(m Money) {
	w.ObjStart()
	w.Key("minor")
	w.Num(m.Minor)
	w.Key("currency")
	w.Str(m.Currency)
	w.ObjEnd()
}

// Blob writes base64 binary data.
func (w *W) Blob(b []byte) { w.Str(base64.StdEncoding.EncodeToString(b)) }

// Raw writes pre-encoded JSON, validating that it parses.
func (w *W) Raw(raw []byte) error {
	if !isValidJSON(raw) {
		return fmt.Errorf("ks: raw JSON value is not valid")
	}
	w.B = append(w.B, raw...)
	return nil
}

// ArrStart opens an array.
func (w *W) ArrStart() { w.B = append(w.B, '['); w.deep++ }

// ArrEnd closes an array.
func (w *W) ArrEnd() { w.B = append(w.B, ']'); w.deep-- }

// Item separates two array elements.
func (w *W) Item() {
	if last := w.B[len(w.B)-1:]; len(w.B) > 0 && last[0] != '[' {
		w.B = append(w.B, ',')
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}


// AppendJSONString appends s as a quoted JSON string, escaping minimally.
const hexLower = "0123456789abcdef"

func AppendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				dst = append(dst, `\`...)
				dst = append(dst, '"')
			case '\\':
				dst = append(dst, `\\`...)
			case '\n':
				dst = append(dst, `\n`...)
			case '\r':
				dst = append(dst, `\r`...)
			case '\t':
				dst = append(dst, `\t`...)
			case '<', '>', '&':
				// As encoding/json does, so a response is the same bytes whichever
				// encoder wrote it, and safe to embed in HTML.
				dst = append(dst, `\u00`...)
				dst = append(dst, hexLower[c>>4], hexLower[c&0xf])
			default:
				if c < 0x20 {
					dst = append(dst, `\u00`...)
					dst = append(dst, hexLower[c>>4], hexLower[c&0xf])
				} else {
					dst = append(dst, c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, `\ufffd`...)
			i++
			continue
		}
		if r == '\u2028' || r == '\u2029' { // line separators break JavaScript string literals
			dst = append(dst, `\u202`...)
			dst = append(dst, hexLower[r&0xf])
			i += size
			continue
		}
		dst = append(dst, s[i:i+size]...)
		i += size
	}
	return append(dst, '"')
}

func isValidJSON(b []byte) bool {
	v, err := ParseValue(b)
	return err == nil && v != nil
}

// ---------- decoder (strict, no reflection) ----------

// Kind enumerates decoded JSON value shapes.
type Kind int

const (
	KindNull Kind = iota
	KindBool
	KindNum
	KindStr
	KindArr
	KindObj
)

// Val is a parsed JSON value. Generated decoders read typed fields out of it.
type Val struct {
	Kind     Kind
	S        string // string contents, or number text
	B        bool
	Arr      []*Val
	obj      map[string]*Val
	keyOrder []string
}

// Body is a parsed JSON object.
type Body = Val

// Parse parses a JSON document.
func Parse(b []byte) (*Val, error) {
	p := &parser{s: b}
	p.ws()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, fmt.Errorf("ks: unexpected trailing data at offset %d", p.i)
	}
	return v, nil
}

// ParseObject parses a document that must be a JSON object.
func ParseObject(b []byte) (*Val, error) {
	v, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindObj {
		return nil, fmt.Errorf("ks: expected a JSON object")
	}
	return v, nil
}

// ParseValue is Parse for callers that hold raw bytes.
func ParseValue(b []byte) (*Val, error) { return Parse(b) }

// Get returns a member of an object, or nil.
func (v *Val) Get(k string) *Val {
	if v == nil || v.Kind != KindObj {
		return nil
	}
	return v.obj[k]
}

// Has reports object membership.
func (v *Val) Has(k string) bool { return v.Get(k) != nil }

// Keys returns object keys in document order.
func (v *Val) Keys() []string {
	if v == nil || v.Kind != KindObj {
		return nil
	}
	return v.keyOrder
}

// Len returns array length.
func (v *Val) Len() int {
	if v == nil {
		return 0
	}
	return len(v.Arr)
}

// At indexes an array.
func (v *Val) At(i int) *Val {
	if v == nil || i >= len(v.Arr) {
		return nil
	}
	return v.Arr[i]
}

// IsNull reports a JSON null.
func (v *Val) IsNull() bool { return v == nil || v.Kind == KindNull }

// Str returns a string value.
func (v *Val) Str() (string, error) {
	if v == nil {
		return "", fmt.Errorf("ks: value is null")
	}
	if v.Kind != KindStr {
		return "", fmt.Errorf("ks: expected a string, got %s", v.kindName())
	}
	if !utf8.ValidString(v.S) {
		return "", fmt.Errorf("ks: string contains invalid UTF-8")
	}
	return v.S, nil
}

// Int returns an integer value, rejecting fractions and overflow.
func (v *Val) Int() (int64, error) {
	if v == nil || v.Kind != KindNum {
		return 0, fmt.Errorf("ks: expected a number")
	}
	n, err := strconv.ParseInt(v.S, 10, 64)
	if err == nil {
		return n, nil
	}
	f, ferr := strconv.ParseFloat(v.S, 64)
	if ferr != nil || f != math.Trunc(f) || math.Abs(f) > 1e15 {
		return 0, fmt.Errorf("ks: %q is not an integer", v.S)
	}
	return int64(f), nil
}

// Float returns a numeric value as a float.
func (v *Val) Float() (float64, error) {
	if v == nil || v.Kind != KindNum {
		return 0, fmt.Errorf("ks: expected a number")
	}
	return strconv.ParseFloat(v.S, 64)
}

// Bool returns a boolean value.
func (v *Val) Bool() (bool, error) {
	if v == nil || v.Kind != KindBool {
		return false, fmt.Errorf("ks: expected a boolean")
	}
	return v.B, nil
}

// Decimal returns an exact numeric value.
func (v *Val) Decimal() (Decimal, error) {
	if v == nil || (v.Kind != KindNum && v.Kind != KindStr) {
		return Decimal{}, fmt.Errorf("ks: expected a decimal")
	}
	return ParseDecimal(v.S)
}

// Time parses an RFC 3339 timestamp.
func (v *Val) Time() (time.Time, error) {
	s, err := v.Str()
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("ks: invalid timestamp %q", s)
	}
	return t.UTC(), nil
}

// ID parses an identifier.
func (v *Val) ID() (ID, error) {
	s, err := v.Str()
	if err != nil {
		return NilID, err
	}
	id := ID(s)
	if !id.IsValid() {
		return NilID, fmt.Errorf("ks: %q is not a uuid", s)
	}
	return id, nil
}

func (v *Val) kindName() string {
	switch v.Kind {
	case KindNull:
		return "null"
	case KindBool:
		return "boolean"
	case KindNum:
		return "number"
	case KindStr:
		return "string"
	case KindArr:
		return "array"
	}
	return "object"
}

// RejectUnknown fails when the object carries keys outside the declared input.
// This is what makes mass assignment unrepresentable (§9.1).
func (v *Val) RejectUnknown(known ...string) error {
	if v == nil || v.Kind != KindObj {
		return fmt.Errorf("ks: expected a JSON object")
	}
	set := make(map[string]bool, len(known))
	for _, k := range known {
		set[k] = true
	}
	var extra []string
	for _, k := range v.keyOrder {
		if !set[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		return Invalid(strings.Join(extra, ","), "unknown field; the input schema does not declare it")
	}
	return nil
}

type parser struct {
	s []byte
	i int
}

const maxDepth = 64

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (*Val, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("ks: JSON nested too deeply")
	}
	p.ws()
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("ks: unexpected end of JSON")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Val{Kind: KindStr, S: s}, nil
	case strings.HasPrefix(string(p.s[p.i:]), "true"):
		p.i += 4
		return &Val{Kind: KindBool, B: true}, nil
	case strings.HasPrefix(string(p.s[p.i:]), "false"):
		p.i += 5
		return &Val{Kind: KindBool}, nil
	case strings.HasPrefix(string(p.s[p.i:]), "null"):
		p.i += 4
		return &Val{Kind: KindNull}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, fmt.Errorf("ks: unexpected character %q at offset %d", p.s[p.i], p.i)
}

func (p *parser) object(depth int) (*Val, error) {
	p.i++ // consume {
	obj := &Val{Kind: KindObj, obj: map[string]*Val{}}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return obj, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, fmt.Errorf("ks: expected a string key at offset %d", p.i)
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, dup := obj.obj[k]; dup {
			return nil, fmt.Errorf("ks: duplicate key %q", k)
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, fmt.Errorf("ks: expected ':' after key %q", k)
		}
		p.i++
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		obj.obj[k] = v
		obj.keyOrder = append(obj.keyOrder, k)
		p.ws()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("ks: unterminated object")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
			continue
		case '}':
			p.i++
			return obj, nil
		}
		return nil, fmt.Errorf("ks: expected ',' or '}' at offset %d", p.i)
	}
}

func (p *parser) array(depth int) (*Val, error) {
	p.i++
	arr := &Val{Kind: KindArr}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return arr, nil
	}
	for {
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		arr.Arr = append(arr.Arr, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("ks: unterminated array")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
			continue
		case ']':
			p.i++
			return arr, nil
		}
		return nil, fmt.Errorf("ks: expected ',' or ']' at offset %d", p.i)
	}
}

func (p *parser) number() (*Val, error) {
	start := p.i
	if p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
	}
	for p.i < len(p.s) {
		c := p.s[p.i]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			p.i++
			continue
		}
		break
	}
	return &Val{Kind: KindNum, S: string(p.s[start:p.i])}, nil
}

func (p *parser) str() (string, error) {
	p.i++ // consume quote
	start := p.i
	for p.i < len(p.s) && p.s[p.i] != '"' {
		if p.s[p.i] == '\\' {
			p.i += 2
			continue
		}
		p.i++
	}
	if p.i >= len(p.s) {
		return "", fmt.Errorf("ks: unterminated string")
	}
	raw := p.s[start:p.i]
	p.i++ // consume closing quote
	if !strings.ContainsRune(string(raw), '\\') {
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("ks: string contains invalid UTF-8")
		}
		return string(raw), nil
	}
	var b strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			b.WriteByte(raw[i])
			i++
			continue
		}
		if i+1 >= len(raw) {
			return "", fmt.Errorf("ks: bad escape")
		}
		switch raw[i+1] {
		case '"':
			b.WriteByte('"')
			i += 2
		case '\\':
			b.WriteByte('\\')
			i += 2
		case '/':
			b.WriteByte('/')
			i += 2
		case 'b':
			b.WriteByte('\b')
			i += 2
		case 'f':
			b.WriteByte('\f')
			i += 2
		case 'n':
			b.WriteByte('\n')
			i += 2
		case 'r':
			b.WriteByte('\r')
			i += 2
		case 't':
			b.WriteByte('\t')
			i += 2
		case 'u':
			if i+5 >= len(raw) {
				return "", fmt.Errorf("ks: bad \\u escape")
			}
			cp, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
			if err != nil {
				return "", fmt.Errorf("ks: bad \\u escape: %w", err)
			}
			r := rune(cp)
			if utf16Surrogate(r) && i+11 < len(raw) && raw[i+6] == '\\' && raw[i+7] == 'u' {
				lo, err := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
				if err == nil && utf16Surrogate(rune(lo)) {
					r = ((r - 0xD800) << 10) | (rune(lo) - 0xDC00) + 0x10000
					i += 12
					b.WriteRune(r)
					continue
				}
			}
			i += 6
			b.WriteRune(r)
		default:
			return "", fmt.Errorf("ks: bad escape \\%c", raw[i+1])
		}
	}
	out := b.String()
	if !utf8.ValidString(out) {
		return "", fmt.Errorf("ks: string contains invalid UTF-8")
	}
	return out, nil
}

func utf16Surrogate(r rune) bool { return r >= 0xD800 && r <= 0xDBFF }
