package runtime

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"keystone.dev/keystone/ks"
	"keystone.dev/keystone/model"
)

// Keyring holds the key-encryption keys for Encrypted() fields (§9.4). Every value is
// sealed with its own random data key (AES-256-GCM), and that data key is sealed with
// the primary key-encryption key. Older keys stay in the ring so values written before
// a rotation still open; ks keys rotate re-seals them under the primary.
//
// A sealed value is: version 1 | len(kid) | kid | nonce | wrapped data key | nonce |
// ciphertext. The table, column and row id are authenticated with it, so a ciphertext
// copied into another row or column does not open.
type Keyring struct {
	primary string
	keys    map[string][]byte
}

// KeyringFromConfig reads the primary key from the encryptionKey secret, or else
// KS_ENCRYPTION_KEY, and retired keys from KS_ENCRYPTION_KEYS_OLD. Each key is written
// "<id>:<base64 of 32 bytes>"; the old list is comma separated. No key gives a nil ring.
func KeyringFromConfig(c *Config) (*Keyring, error) {
	primary := c.Secret("encryptionKey")
	if primary == "" {
		primary = os.Getenv("KS_ENCRYPTION_KEY")
	}
	if primary == "" {
		return nil, nil
	}
	k := &Keyring{keys: map[string][]byte{}}
	id, err := k.add(primary)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	k.primary = id
	for _, old := range strings.Split(os.Getenv("KS_ENCRYPTION_KEYS_OLD"), ",") {
		if strings.TrimSpace(old) == "" {
			continue
		}
		if _, err := k.add(strings.TrimSpace(old)); err != nil {
			return nil, fmt.Errorf("retired encryption key: %w", err)
		}
	}
	return k, nil
}

// NewKey returns a fresh key in the "<id>:<base64>" form the config expects.
func NewKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	// The id names the key inside every value it seals, so it must be unique: a date for
	// people, and random bytes so two keys made in the same second differ.
	return time.Now().UTC().Format("20060102") + "-" + randHex(4) + ":" + base64.StdEncoding.EncodeToString(b)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (k *Keyring) add(spec string) (string, error) {
	id, b64, ok := strings.Cut(spec, ":")
	if !ok || id == "" || len(id) > 255 {
		return "", errors.New(`want "<id>:<base64 of 32 bytes>"`)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != 32 {
		return "", errors.New("the key must be 32 bytes, base64 encoded")
	}
	k.keys[id] = raw
	return id, nil
}

// Primary names the key new values are sealed with.
func (k *Keyring) Primary() string { return k.primary }

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func aad(table, column, rowID string) []byte { return []byte(table + "\x00" + column + "\x00" + rowID) }

// Seal encrypts one value for a table, column and row.
func (k *Keyring) Seal(plain []byte, table, column, rowID string) ([]byte, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	kek, err := gcm(k.keys[k.primary])
	if err != nil {
		return nil, err
	}
	data, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	n1 := make([]byte, kek.NonceSize())
	n2 := make([]byte, data.NonceSize())
	if _, err := rand.Read(n1); err != nil {
		return nil, err
	}
	if _, err := rand.Read(n2); err != nil {
		return nil, err
	}
	out := []byte{1, byte(len(k.primary))}
	out = append(out, k.primary...)
	out = append(out, n1...)
	out = kek.Seal(out, n1, dek, []byte(k.primary))
	out = append(out, n2...)
	return data.Seal(out, n2, plain, aad(table, column, rowID)), nil
}

// KeyOf names the key a sealed value was written with.
func KeyOf(sealed []byte) (string, error) {
	if len(sealed) < 2 || sealed[0] != 1 || len(sealed) < 2+int(sealed[1]) {
		return "", errors.New("runtime: not a sealed value")
	}
	return string(sealed[2 : 2+int(sealed[1])]), nil
}

// Open decrypts a value sealed by Seal for the same table, column and row.
func (k *Keyring) Open(sealed []byte, table, column, rowID string) ([]byte, error) {
	kid, err := KeyOf(sealed)
	if err != nil {
		return nil, err
	}
	key, ok := k.keys[kid]
	if !ok {
		return nil, fmt.Errorf("runtime: value sealed with unknown key %q; add it to KS_ENCRYPTION_KEYS_OLD", kid)
	}
	kek, err := gcm(key)
	if err != nil {
		return nil, err
	}
	rest := sealed[2+len(kid):]
	wrapped := kek.NonceSize() + 32 + kek.Overhead()
	if len(rest) < wrapped {
		return nil, errors.New("runtime: sealed value is truncated")
	}
	dek, err := kek.Open(nil, rest[:kek.NonceSize()], rest[kek.NonceSize():wrapped], []byte(kid))
	if err != nil {
		return nil, errors.New("runtime: the data key does not open with its key-encryption key")
	}
	data, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	rest = rest[wrapped:]
	if len(rest) < data.NonceSize() {
		return nil, errors.New("runtime: sealed value is truncated")
	}
	plain, err := data.Open(nil, rest[:data.NonceSize()], rest[data.NonceSize():], aad(table, column, rowID))
	if err != nil {
		return nil, errors.New("runtime: the value does not open for this row and column")
	}
	return plain, nil
}

// plainText renders a value an action assigns to an encrypted column; reads hand the
// text back to the generated scanner, which parses it like any text column.
func plainText(v any) ([]byte, bool) {
	switch t := v.(type) {
	case nil:
		return nil, false
	case *string:
		if t == nil {
			return nil, false
		}
		return []byte(*t), true
	case string:
		return []byte(t), true
	case []byte:
		return t, true
	case time.Time:
		return []byte(t.UTC().Format(time.RFC3339Nano)), true
	case *time.Time:
		if t == nil {
			return nil, false
		}
		return []byte(t.UTC().Format(time.RFC3339Nano)), true
	case fmt.Stringer:
		return []byte(t.String()), true
	}
	return []byte(fmt.Sprint(v)), true
}

func (v *View[T]) encrypted(column string) bool {
	for _, c := range v.t.Columns {
		if c.Column == column {
			return c.Encrypted
		}
	}
	return false
}

// seal encrypts a value bound for an Encrypted() column, and passes any other value
// through. A missing keyring is a startup error, so it cannot be reached here.
func (v *View[T]) seal(column string, id ks.ID, val any) any {
	if !v.encrypted(column) {
		return val
	}
	plain, ok := plainText(val)
	if !ok {
		return nil
	}
	sealed, err := v.db.keys.Seal(plain, v.t.Table, column, string(id))
	if err != nil {
		panic("runtime: sealing " + column + ": " + err.Error())
	}
	return sealed
}

// scanner opens the encrypted columns of each row before the generated scanner reads
// it, so action code only ever sees plaintext (§9.4).
func (v *View[T]) scanner() func(Row) (*T, error) {
	var idx []int
	for i, c := range v.t.Columns {
		if c.Encrypted {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return v.t.Scan
	}
	return func(r Row) (*T, error) {
		rr, ok := r.(*row)
		if !ok {
			return v.t.Scan(r)
		}
		id := string(rr.ID(0))
		for _, i := range idx {
			sealed, ok := rr.at(i).([]byte)
			if !ok || sealed == nil {
				continue // NULL, or hidden by a field rule
			}
			plain, err := v.db.keys.Open(sealed, v.t.Table, v.t.Columns[i].Column, id)
			if err != nil {
				return nil, err
			}
			rr.v[i] = string(plain)
		}
		return v.t.Scan(r)
	}
}

func hasEncrypted(a *model.App) bool {
	if a == nil {
		return false
	}
	for _, r := range a.AllResources() {
		for _, f := range r.Resource.Fields {
			if f.Type.Encrypted {
				return true
			}
		}
	}
	return false
}
