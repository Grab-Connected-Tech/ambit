// Package ks holds the public scalar types, actor model, clock and error vocabulary
// of the Keystone framework. It is imported by every generated and hand-written
// action package, so it must stay free of runtime and database dependencies.
package ks

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ID is the framework's identifier type: a lowercase UUIDv7 string.
// Generated code declares typed aliases over it (type InvoiceID ks.ID).
type ID string

// NilID is the zero identifier. It never matches a stored row.
const NilID ID = ""

// NewID returns a UUIDv7 (time-ordered) identifier.
func NewID() ID {
	var b [16]byte
	ts := uint64(time.Now().UnixMilli())
	copy(b[0:6], tsBytes(ts))
	_, _ = rand.Read(b[6:16])
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return ID(formatUUID(b))
}

func tsBytes(ts uint64) []byte {
	out := make([]byte, 6)
	for i := 0; i < 6; i++ {
		out[i] = byte(ts >> ((5 - i) * 8))
	}
	return out
}

// IDFromBytes builds an ID from a 16-byte uuid.
func IDFromBytes(b []byte) (ID, error) {
	if len(b) != 16 {
		return NilID, fmt.Errorf("ks: uuid must be 16 bytes, got %d", len(b))
	}
	return ID(formatUUID(*(*[16]byte)(b))), nil
}

// Bytes returns the 16-byte uuid form, or nil when malformed.
func (i ID) Bytes() []byte {
	hex := strings.ReplaceAll(string(i), "-", "")
	if len(hex) != 32 {
		return nil
	}
	out := make([]byte, 16)
	for k := 0; k < 16; k++ {
		v, err := strconv.ParseUint(hex[k*2:k*2+2], 16, 8)
		if err != nil {
			return nil
		}
		out[k] = byte(v)
	}
	return out
}

func (i ID) String() string { return string(i) }

// IsValid reports a well-formed uuid string.
func (i ID) IsValid() bool {
	b := i.Bytes()
	return b != nil
}

// IsNil reports the zero identifier.
func (i ID) IsNil() bool { return i == "" || i == NilID }

// OrNil renders the id for SQL parameters; empty ids become NULL.
func (i ID) OrNil() any {
	if i == "" {
		return nil
	}
	return string(i)
}

func formatUUID(b [16]byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 36)
	pos := 0
	for i, v := range b {
		out[pos] = hexDigits[v>>4]
		out[pos+1] = hexDigits[v&0x0f]
		pos += 2
		if i == 3 || i == 5 || i == 7 || i == 9 {
			out[pos] = '-'
			pos++
		}
	}
	return string(out)
}

// Opt is an optional field value. The zero means "absent", which is distinct from
// "present with the zero value"; patches use it so an update never guesses.
type Opt[T any] struct {
	value T
	set   bool
}

// Set returns an Opt holding v.
func Set[T any](v T) Opt[T] { return Opt[T]{value: v, set: true} }

// Unset returns an absent Opt.
func Unset[T any]() Opt[T] { return Opt[T]{} }

// OptOf wraps a pointer, producing an absent Opt when nil.
func OptOf[T any](p *T) Opt[T] {
	if p == nil {
		return Opt[T]{}
	}
	return Set(*p)
}

// Present reports whether the value was set.
func (o Opt[T]) Present() bool { return o.set }

// Get returns the value and whether it was set.
func (o Opt[T]) Get() (T, bool) { return o.value, o.set }

// MustGet returns the value or panics; generated validators prevent the absent case.
func (o Opt[T]) MustGet() T {
	if !o.set {
		panic(errors.New("ks: Opt.Get on absent value"))
	}
	return o.value
}

// Or returns the value or def when absent.
func (o Opt[T]) Or(def T) T {
	if o.set {
		return o.value
	}
	return def
}

// MarshalJSON writes the value, or null when absent.
func (o Opt[T]) MarshalJSON() ([]byte, error) {
	if !o.set {
		return []byte("null"), nil
	}
	return json.Marshal(o.value)
}

// UnmarshalJSON reads a value; null leaves the Opt absent, as a missing key does.
func (o *Opt[T]) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*o = Opt[T]{}
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*o = Set(v)
	return nil
}

// Ptr returns a pointer to the value, or nil when absent.
func (o Opt[T]) Ptr() *T {
	if !o.set {
		return nil
	}
	v := o.value
	return &v
}
