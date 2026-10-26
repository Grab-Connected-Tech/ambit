package policy

import (
	"fmt"
	"strings"
	"time"
)

// ActorInfo is the caller facts an in-Go evaluation sees.
type ActorInfo struct {
	ID       any
	Tenant   any
	Roles    []string
	IsSystem bool
	Now      time.Time
}

// Row is one record's field values, keyed by field name.
type Row map[string]any

// Env is the evaluation context: a row, its related rows and the actor.
type Env struct {
	Row  Row
	Refs map[string]Row // relation field name -> loaded related row
	A    ActorInfo
}

// Eval is the Go interpretation of a rule. SQL remains the authority; Eval exists
// so the diff, generated-test scaffolds and property checks can reason about a
// rule without a database connection.
func Eval(n *Node, e *Env) (bool, error) {
	if n == nil {
		return false, fmt.Errorf("policy: eval of nil node")
	}
	switch n.Op {
	case OpAlways:
		return true, nil
	case OpNever:
		return false, nil
	case OpAnd:
		for _, k := range n.Kids {
			v, err := Eval(k, e)
			if err != nil {
				return false, err
			}
			if !v {
				return false, nil
			}
		}
		return true, nil
	case OpOr:
		anyErr := error(nil)
		for _, k := range n.Kids {
			v, err := Eval(k, e)
			if err != nil {
				anyErr = err
				continue
			}
			if v {
				return true, nil
			}
		}
		return false, anyErr
	case OpNot:
		v, err := Eval(n.Kids[0], e)
		return !v, err
	case OpImplies:
		a, err := Eval(n.Kids[0], e)
		if err != nil {
			return false, err
		}
		if !a {
			return true, nil
		}
		return Eval(n.Kids[1], e)
	case OpRole:
		if e.A.IsSystem {
			return true, nil
		}
		for _, want := range n.Roles {
			for _, got := range e.A.Roles {
				if got == want {
					return true, nil
				}
			}
		}
		return false, nil
	case OpOwner:
		v := fieldOf(e, n.Path)
		return eq(v, e.A.ID), nil
	case OpIsSet:
		x, err := value(n.Kids[0], e)
		if err != nil {
			return false, err
		}
		return !isNull(x), nil
	case OpIsNull:
		x, err := value(n.Kids[0], e)
		if err != nil {
			return false, err
		}
		return isNull(x), nil
	case OpIn:
		x, err := value(n.Kids[0], e)
		if err != nil {
			return false, err
		}
		list := n.Kids[1].Values[0]
		for _, v := range list.List {
			g, err := litGo(v)
			if err != nil {
				return false, err
			}
			if eq(x, g) {
				return true, nil
			}
		}
		return false, nil
	case OpPrefix:
		l, err := value(n.Kids[0], e)
		if err != nil {
			return false, err
		}
		r, err := value(n.Kids[1], e)
		if err != nil {
			return false, err
		}
		ls, ok1 := l.(string)
		rs, ok2 := r.(string)
		if !ok1 || !ok2 {
			return false, nil
		}
		return strings.HasPrefix(ls, rs), nil
	case OpEq:
		return bin(n, e, func(c int) bool { return c == 0 }, true)
	case OpNe:
		return bin(n, e, func(c int) bool { return c != 0 }, false)
	case OpGt:
		return bin(n, e, func(c int) bool { return c > 0 }, false)
	case OpGte:
		return bin(n, e, func(c int) bool { return c >= 0 }, false)
	case OpLt:
		return bin(n, e, func(c int) bool { return c < 0 }, false)
	case OpLte:
		return bin(n, e, func(c int) bool { return c <= 0 }, false)
	}
	return false, fmt.Errorf("policy: cannot evaluate op %q", n.Op)
}

func bin(n *Node, e *Env, test func(int) bool, nullEq bool) (bool, error) {
	l, err := value(n.Kids[0], e)
	if err != nil {
		return false, err
	}
	r, err := value(n.Kids[1], e)
	if err != nil {
		return false, err
	}
	if isNull(l) || isNull(r) {
		// SQL semantics: an unknown comparison is not true. <> is the exception,
		// which compiles to IS DISTINCT FROM.
		if n.Op == OpNe {
			return isNull(l) != isNull(r) || (!isNull(l) && !isNull(r) && !eq(l, r)), nil
		}
		return false, nil
	}
	c, err := compare(l, r)
	if err != nil {
		return false, err
	}
	return test(c), nil
}

func value(n *Node, e *Env) (any, error) {
	switch n.Op {
	case OpField:
		return fieldOf(e, n.Path), nil
	case OpLit:
		return litGo(n.Values[0])
	case OpActorID:
		return e.A.ID, nil
	case OpActorTenant:
		return e.A.Tenant, nil
	case OpNow:
		return e.A.Now, nil
	case OpAlways:
		return true, nil
	case OpNever:
		return false, nil
	case OpRole:
		if e.A.IsSystem {
			return true, nil
		}
		for _, want := range n.Roles {
			for _, got := range e.A.Roles {
				if got == want {
					return true, nil
				}
			}
		}
		return false, nil
	}
	return Eval(n, e)
}

func fieldOf(e *Env, path []string) any {
	if len(path) == 1 {
		return e.Row[path[0]]
	}
	if ref, ok := e.Refs[path[0]]; ok {
		return ref[path[1]]
	}
	return nil
}

func isNull(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return false && t == ""
	case *string:
		return t == nil
	}
	return v == nil
}

func eq(a, b any) bool {
	if isNull(a) || isNull(b) {
		return isNull(a) && isNull(b)
	}
	c, err := compare(a, b)
	return err == nil && c == 0
}

// compare orders two scalars across the numeric and string types the spec allows.
func compare(a, b any) (int, error) {
	if x, ok := asFloat(a); ok {
		if y, ok := asFloat(b); ok {
			switch {
			case x < y:
				return -1, nil
			case x > y:
				return 1, nil
			default:
				return 0, nil
			}
		}
	}
	if x, ok := a.(bool); ok {
		if y, ok := b.(bool); ok {
			switch {
			case x == y:
				return 0, nil
			case !x:
				return -1, nil
			default:
				return 1, nil
			}
		}
	}
	if x, ok := a.(time.Time); ok {
		if y, ok := b.(time.Time); ok {
			switch {
			case x.Before(y):
				return -1, nil
			case x.After(y):
				return 1, nil
			default:
				return 0, nil
			}
		}
	}
	as, aok := text(a)
	bs, bok := text(b)
	if aok && bok {
		return strings.Compare(as, bs), nil
	}
	if aok && !bok {
		return strings.Compare(as, fmt.Sprint(b)), nil
	}
	if !aok && bok {
		return strings.Compare(fmt.Sprint(a), bs), nil
	}
	return 0, fmt.Errorf("policy: cannot compare %T with %T", a, b)
}

func text(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case []byte:
		return string(t), true
	}
	return "", false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case string:
		f, err := strconvFloat(t)
		return f, err == nil
	}
	return 0, false
}
