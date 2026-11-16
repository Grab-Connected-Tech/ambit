package amtest

import (
	"regexp"
	"testing"
	"time"

	"github.com/Grab-Connected-Tech/ambit/model"
)

// Fixtures must satisfy the constraints a spec declares, or a correct spec fails its own
// generated tests (found by an agent that dropped its constraints to get past them).
func TestPlaceholdersSatisfyDeclaredConstraints(t *testing.T) {
	three := 3
	for _, p := range []string{`^[A-Z]{3}$`, `^\d{4}-\d{2}$`, `^(USD|EUR)$`, `^[a-z]+@[a-z]+\.test$`, `^x?y*z+$`} {
		f := model.Field{Name: "code", Type: model.Type{Kind: model.KindText, Pattern: p}}
		v, _ := placeholder(f, time.Now())
		if !regexp.MustCompile(p).MatchString(v.(string)) {
			t.Errorf("placeholder %q does not match %s", v, p)
		}
	}
	short := model.Field{Name: "description", Type: model.Type{Kind: model.KindText, MaxLen: &three}}
	if v, _ := placeholder(short, time.Now()); len(v.(string)) > 3 {
		t.Errorf("placeholder %q is longer than MaxLen 3", v)
	}
	long := model.Field{Name: "x", Type: model.Type{Kind: model.KindText, MinLen: &three, MaxLen: &three}}
	if v, _ := placeholder(long, time.Now()); len(v.(string)) != 3 {
		t.Errorf("placeholder %q does not fit MinLen=MaxLen=3", v)
	}
	positive := model.Field{Name: "qty", Type: model.Type{Kind: model.KindInt, Min: "1"}}
	if v, _ := placeholder(positive, time.Now()); v.(int) < 1 {
		t.Errorf("placeholder %v is below Min 1", v)
	}
	negative := model.Field{Name: "delta", Type: model.Type{Kind: model.KindInt, Max: "-5"}}
	if v, _ := placeholder(negative, time.Now()); v.(int) > -5 {
		t.Errorf("placeholder %v is above Max -5", v)
	}
}
