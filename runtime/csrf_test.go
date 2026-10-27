package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A cookie-authenticated write from another origin is refused before the session is read.
func TestAuthenticateRejectsCrossOriginCookieWrite(t *testing.T) {
	r := httptest.NewRequest("POST", "http://app.example/x", nil)
	r.Header.Set("Origin", "https://evil.example")
	r.AddCookie(&http.Cookie{Name: "am_session", Value: "tok"})
	if _, err := (&App{}).Authenticate(r); err == nil {
		t.Fatal("cross-origin cookie POST was accepted")
	}
}
