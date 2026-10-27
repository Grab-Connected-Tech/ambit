package runtime

import (
	"bytes"
	"os"
	"testing"
)

func TestKeyringSealsPerRowAndRotates(t *testing.T) {
	old, cur := NewKey(), NewKey()
	os.Setenv("AM_ENCRYPTION_KEY", old)
	k1, err := KeyringFromConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := k1.Seal([]byte("DE123"), "invoice", "tax_id", "row-1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("DE123")) {
		t.Fatal("the plaintext is visible in the sealed value")
	}
	if _, err := k1.Open(sealed, "invoice", "tax_id", "row-2"); err == nil {
		t.Fatal("a value copied to another row must not open")
	}
	if _, err := k1.Open(sealed, "invoice", "notes", "row-1"); err == nil {
		t.Fatal("a value copied to another column must not open")
	}
	// After rotation the old key is retired but still opens what it sealed.
	os.Setenv("AM_ENCRYPTION_KEY", cur)
	os.Setenv("AM_ENCRYPTION_KEYS_OLD", old)
	defer os.Unsetenv("AM_ENCRYPTION_KEY")
	defer os.Unsetenv("AM_ENCRYPTION_KEYS_OLD")
	k2, err := KeyringFromConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := k2.Open(sealed, "invoice", "tax_id", "row-1")
	if err != nil || string(plain) != "DE123" {
		t.Fatalf("open with a retired key: %q %v", plain, err)
	}
	resealed, _ := k2.Seal(plain, "invoice", "tax_id", "row-1")
	if kid, _ := KeyOf(resealed); kid != k2.Primary() {
		t.Fatalf("new values use the primary key, got %s", kid)
	}
}
