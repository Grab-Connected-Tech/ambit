package policy

import (
	"sort"
	"strconv"
	"strings"
)

func strconvFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

// Relation describes how a new rule compares to an old one (§15.2).
type Relation int

const (
	// Equal means no behavioral change.
	Equal Relation = iota
	// Narrowed means the new rule accepts a strict subset of the old one.
	Narrowed
	// Widened means the new rule accepts something the old one refused.
	Widened
	// Unknown means coverage could not be proven either way. It is reported as
	// widened: false alarms are acceptable, missed widenings are not.
	Unknown
)

func (r Relation) String() string {
	switch r {
	case Equal:
		return "equal"
	case Narrowed:
		return "narrowed"
	case Widened:
		return "widened"
	}
	return "widened (unproven)"
}

// Compare classifies a rule change.
func Compare(old, new *Node) (Relation, string) { return compareAt(old, new, 0) }

func compareAt(old, new *Node, depth int) (Relation, string) {
	if depth > 12 {
		return Unknown, "rules differ in a shape that comparison cannot follow"
	}
	if old == nil && new == nil {
		return Equal, ""
	}
	if old == nil {
		return Widened, "rule added where access was denied"
	}
	if new == nil {
		return Narrowed, "rule removed, access falls back to deny"
	}
	oc, nc := Canonical(old), Canonical(new)
	if oc == nc {
		return Equal, ""
	}
	// covers(wide, narrow) means "wide accepts at least what narrow accepts".
	if covers(new, old) {
		return Widened, describe(old, new, "loosened")
	}
	if covers(old, new) {
		return Narrowed, describe(old, new, "tightened")
	}
	return Unknown, describe(old, new, "changed")
}

// covers reports whether every actor and row that satisfies narrow also satisfies
// wide. It is deliberately incomplete and always safe in one direction.
func covers(wide, narrow *Node) bool {
	if wide == nil || narrow == nil {
		return wide != nil && wide.Op == OpAlways
	}
	if Canonical(wide) == Canonical(narrow) {
		return true
	}
	if wide.Op == OpAlways {
		return true
	}
	if narrow.Op == OpNever {
		return true
	}
	if wide.Op == OpNever || narrow.Op == OpAlways {
		return false
	}
	switch {
	case wide.Op == OpOr:
		if narrow.Op == OpOr {
			for _, b := range narrow.Kids {
				if !anyCovers(wide.Kids, b) {
					return false
				}
			}
			return true
		}
		return anyCovers(wide.Kids, narrow)
	case narrow.Op == OpOr:
		// A disjunction below is the union of its branches, so the wider side has
		// to cover every one of them.
		for _, b := range narrow.Kids {
			if !covers(b, wide) {
				return false
			}
		}
		return true
	case wide.Op == OpAnd:
		// A conjunction accepts less than any single conjunct, so every conjunct
		// on the wide side must still cover the narrow side.
		for _, a := range wide.Kids {
			if narrow.Op == OpAnd {
				if !anyCovers(narrow.Kids, a) {
					return false
				}
				continue
			}
			if !covers(a, narrow) {
				return false
			}
		}
		return true
	case narrow.Op == OpAnd:
		// A conjunction below is at most as wide as any single conjunct.
		return anyCovers(narrow.Kids, wide)
	case wide.Op == OpRole && narrow.Op == OpRole:
		return subset(narrow.Roles, wide.Roles)
	case wide.Op == OpNot && narrow.Op == OpNot:
		return covers(narrow.Kids[0], wide.Kids[0])
	}
	return coversLeaf(wide, narrow)
}

func anyCovers(cands []*Node, target *Node) bool {
	for _, c := range cands {
		if covers(c, target) {
			return true
		}
	}
	return false
}

// coversLeaf handles same-field predicates, which is where most real widenings live.
func coversLeaf(wide, narrow *Node) bool {
	if wide.Op != narrow.Op || len(wide.Kids) != 2 || len(narrow.Kids) != 2 {
		return false
	}
	wf, nl := wide.Kids[0], narrow.Kids[1]
	if wf.Op != OpField || nl.Op != OpLit {
		return false
	}
	if Canonical(wf) != Canonical(narrow.Kids[0]) {
		return false
	}
	switch wide.Op {
	case OpEq:
		return Canonical(narrow.Kids[1]) == Canonical(wide.Kids[1])
	case OpIn:
		wl, ok1 := listSet(wide.Kids[1])
		nl2, ok2 := listSet(narrow.Kids[1])
		return ok1 && ok2 && subset(nl2, wl)
	}
	wv, wok := num(wide.Kids[1].Values[0])
	nv, nok := num(narrow.Kids[1].Values[0])
	if !wok || !nok {
		return false
	}
	switch wide.Op + "|" + narrow.Op {
	case "gt|gt", "gte|gte", "lt|lt", "lte|lte":
		if wide.Op == "gt" || wide.Op == "gte" {
			return wv <= nv
		}
		return wv >= nv
	case "gt|gte":
		return wv <= nv
	case "gte|gt":
		return wv < nv
	case "lt|lte":
		return wv >= nv
	case "lte|lt":
		return wv > nv
	}
	return false
}

func listSet(n *Node) ([]string, bool) {
	if len(n.Values) == 0 || n.Values[0].Kind != "list" {
		return nil, false
	}
	out := make([]string, 0, len(n.Values[0].List))
	for _, v := range n.Values[0].List {
		out = append(out, v.String())
	}
	sort.Strings(out)
	return out, true
}

func num(l Lit) (float64, bool) {
	if l.Kind != "int" && l.Kind != "decimal" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimPrefix(l.Text, "+"), 64)
	return f, err == nil
}

func subset(a, b []string) bool {
	in := make(map[string]bool, len(b))
	for _, x := range b {
		in[x] = true
	}
	for _, x := range a {
		if !in[x] {
			return false
		}
	}
	return true
}

func describe(old, new *Node, verb string) string {
	o, n := Canonical(old), Canonical(new)
	diff := explainDelta(old, new)
	if diff == "" {
		return verb + ": " + o + " -> " + n
	}
	return verb + ": " + diff
}

// explainDelta names the concrete difference when one is obvious, which is what
// makes the behavior diff readable (§15.3).
func explainDelta(old, new *Node) string {
	if old.Op == OpRole && new.Op == OpRole {
		added := minus(new.Roles, old.Roles)
		removed := minus(old.Roles, new.Roles)
		switch {
		case len(added) > 0 && len(removed) == 0:
			return strings.Join(added, ",") + " gained access"
		case len(removed) > 0 && len(added) == 0:
			return strings.Join(removed, ",") + " lost access"
		}
	}
	if old.Op == OpNever && new.Op == OpAlways {
		return "rule replaced with always"
	}
	if old.Op == OpAlways && new.Op == OpNever {
		return "rule replaced with never"
	}
	if old.Op == OpOr && new.Op == OpOr {
		if added := orAdded(old, new); added != "" {
			return added
		}
	}
	if old.Op == OpAnd && new.Op == OpAnd {
		if dropped := andDropped(old, new); dropped != "" {
			return dropped
		}
	}
	if sameFieldCond(old, new) {
		return "condition widened on " + strings.Join(old.Kids[0].Path, ".")
	}
	return ""
}

func sameFieldCond(a, b *Node) bool {
	return a.Op == b.Op && len(a.Kids) == 2 && a.Kids[0].Op == OpField &&
		Canonical(a.Kids[0]) == Canonical(b.Kids[0])
}

func orAdded(old, new *Node) string {
	var out []string
	for _, k := range new.Kids {
		found := false
		for _, o := range old.Kids {
			if Canonical(o) == Canonical(k) || covers(k, o) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, k.String())
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "new branch: " + strings.Join(out, " | ")
}

func andDropped(old, new *Node) string {
	for _, o := range old.Kids {
		found := false
		for _, k := range new.Kids {
			if Canonical(o) == Canonical(k) {
				found = true
				break
			}
		}
		if !found {
			return "clause removed: " + o.String()
		}
	}
	return ""
}

func minus(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
