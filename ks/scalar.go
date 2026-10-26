package ks

import (
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// IDArg converts an ID into a value pgx binds to a uuid column.
func IDArg(id ID) any {
	if id.IsNil() {
		return nil
	}
	return id.Bytes()
}

// ScanID builds an ID from a driver value (uuid bytes or text).
func ScanID(v any) (ID, error) {
	switch t := v.(type) {
	case nil:
		return NilID, nil
	case []byte:
		return IDFromBytes(t)
	case string:
		return ID(t), nil
	case [16]byte:
		return IDFromBytes(t[:])
	}
	return NilID, fmt.Errorf("ks: cannot scan %T as ID", v)
}

// Money is an exact monetary amount in minor units with a currency code.
type Money struct {
	Minor    int64
	Currency string
}

// MoneyOf builds a Money value.
func MoneyOf(minor int64, currency string) Money {
	return Money{Minor: minor, Currency: strings.ToUpper(currency)}
}

// ZeroMoney returns a currency-only skeleton used by decoders.
func ZeroMoney(currency string) Money { return Money{Currency: strings.ToUpper(currency)} }

func (m Money) String() string {
	sign := ""
	v := m.Minor
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, v/100, v%100, m.Currency)
}

// IsValid reports a sane currency code.
func (m Money) IsValid() bool {
	re := regexp.MustCompile(`^[A-Z]{3}$`)
	return re.MatchString(m.Currency)
}

// Cmp orders money by minor units; different currencies compare by code afterwards.
func (m Money) Cmp(o Money) int {
	if m.Minor != o.Minor {
		if m.Minor < o.Minor {
			return -1
		}
		return 1
	}
	return strings.Compare(m.Currency, o.Currency)
}

// Decimal is an exact fixed-point number backed by big.Rat for arithmetic and by
// canonical text for storage.
type Decimal struct{ r *big.Rat }

// DecimalOf parses "12.5"; it panics on malformed input (use ParseDecimal in generators).
func DecimalOf(s string) Decimal {
	d, err := ParseDecimal(s)
	if err != nil {
		panic(err)
	}
	return d
}

// ParseDecimal accepts an optional sign, digits and a fractional part.
func ParseDecimal(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Decimal{}, fmt.Errorf("ks: empty decimal")
	}
	if !decimalRe.MatchString(s) {
		return Decimal{}, fmt.Errorf("ks: invalid decimal %q", s)
	}
	r := new(big.Rat)
	if _, ok := r.SetString(s); !ok {
		return Decimal{}, fmt.Errorf("ks: invalid decimal %q", s)
	}
	return Decimal{r: r}, nil
}

// DecimalInt builds a decimal from an integer.
func DecimalInt(i int64) Decimal { return Decimal{r: new(big.Rat).SetInt64(i)} }

var decimalRe = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?$`)

func (d Decimal) rat() *big.Rat {
	if d.r == nil {
		return new(big.Rat)
	}
	return d.r
}

// String renders the shortest exact decimal form.
func (d Decimal) String() string {
	return d.rat().FloatString(12)
}

// Float64 is lossy and only used for metrics and bounds checks.
func (d Decimal) Float64() float64 { f, _ := d.rat().Float64(); return f }

// Cmp compares two decimals.
func (d Decimal) Cmp(o Decimal) int { return d.rat().Cmp(o.rat()) }

// Add returns the sum.
func (d Decimal) Add(o Decimal) Decimal { return Decimal{r: new(big.Rat).Add(d.rat(), o.rat())} }

// Date is a calendar date without a time zone.
type Date struct {
	y    int16
	m, d uint8
}

// DateOf builds a Date.
func DateOf(y int, m, d int) Date { return Date{y: int16(y), m: uint8(m), d: uint8(d)} }

// ParseDate accepts YYYY-MM-DD.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("ks: invalid date %q", s)
	}
	return DateOf(t.Year(), int(t.Month()), t.Day()), nil
}

// DateFromTime truncates a time to its UTC calendar date.
func DateFromTime(t time.Time) Date { u := t.UTC(); return DateOf(u.Year(), int(u.Month()), u.Day()) }

func (d Date) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.y, d.m, d.d) }

// Time returns the date at UTC midnight.
func (d Date) Time() time.Time {
	return time.Date(int(d.y), time.Month(d.m), int(d.d), 0, 0, 0, 0, time.UTC)
}

// Cmp orders dates.
func (d Date) Cmp(o Date) int { return strings.Compare(d.String(), o.String()) }

// Valid reports whether the calendar fields are in range.
func (d Date) Valid() bool {
	return d.m >= 1 && d.m <= 12 && d.d >= 1 && d.d <= 31
}

// ScanDate converts a driver time.Time into a Date.
func ScanDate(t time.Time) Date { return DateFromTime(t) }

// Email is a normalized (lowercased) email address.
type Email string

var emailRe = regexp.MustCompile(`^[a-z0-9!#$%&'*+/=?^_` + "`" + `{|}~.-]+@[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// NormalizeEmail lowercases and validates an address.
func NormalizeEmail(s string) (Email, error) {
	e := Email(strings.ToLower(strings.TrimSpace(s)))
	if !emailRe.MatchString(string(e)) {
		return "", fmt.Errorf("ks: invalid email %q", s)
	}
	return e, nil
}

func (e Email) String() string { return string(e) }

// Valid reports whether the address is well formed.
func (e Email) Valid() bool { return emailRe.MatchString(string(e)) }

// URL is a validated absolute URL.
type URL string

var urlSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true, "mailto": true, "s3": true}

// ParseURL validates an absolute URL, optionally restricted to schemes.
func ParseURL(s string, schemes ...string) (URL, error) {
	u := URL(strings.TrimSpace(s))
	i := strings.Index(string(u), "://")
	if i <= 0 {
		return "", fmt.Errorf("ks: invalid url %q", s)
	}
	scheme := strings.ToLower(string(u)[:i])
	if len(schemes) == 0 {
		if !urlSchemes[scheme] {
			return "", fmt.Errorf("ks: url scheme %q not allowed", scheme)
		}
		return u, nil
	}
	for _, allow := range schemes {
		if strings.EqualFold(allow, scheme) {
			return u, nil
		}
	}
	return "", fmt.Errorf("ks: url scheme %q not allowed", scheme)
}

func (u URL) String() string { return string(u) }

// Host returns the authority component, without port.
func (u URL) Host() string {
	s := string(u)
	i := strings.Index(s, "://")
	if i < 0 {
		return ""
	}
	rest := s[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if c := strings.LastIndex(rest, ":"); c >= 0 && !strings.Contains(rest[c:], "]") {
		rest = rest[:c]
	}
	return strings.ToLower(rest)
}

// Version is the optimistic concurrency counter on every resource.
type Version int64

// Int64 renders the version for parameters.
func (v Version) Int64() int64 { return int64(v) }

func (v Version) String() string { return strconv.FormatInt(int64(v), 10) }
