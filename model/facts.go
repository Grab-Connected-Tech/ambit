package model

import (
	"encoding/json"
	"sort"
)

// Facts are the code-level truths the checker extracts from action packages (§6.3).
// They live beside the model, never inside it, so the diff can report code risk
// without re-analyzing an old revision.
type Facts struct {
	App       string       `json:"app"`
	ModelHash string       `json:"modelHash"`
	Generated string       `json:"generated"`
	Unsafe    []UnsafeFact `json:"unsafe,omitempty"`
	Allows    []AllowFact  `json:"allows,omitempty"`
	Deps      []Dep        `json:"deps,omitempty"`
	Imports   []string     `json:"imports,omitempty"`
	Queries   []QueryFact  `json:"queries,omitempty"`
	Covered   []string     `json:"coveredActions,omitempty"`
}

// UnsafeFact records one escape hatch, with the reason the author gave (§8.6).
type UnsafeFact struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Module string `json:"module"`
	Action string `json:"action"`
	Kind   string `json:"kind"` // raw_sql|banned_import|suppression|cross_tenant
	Reason string `json:"reason"`
}

// Key identifies a fact across revisions. The line moves when a file is edited, so
// identity is the action and the kind, not the position.
func (u UnsafeFact) Key() string { return u.Module + "." + u.Action + "|" + u.Kind }

// AllowFact records one //ks:allow suppression with its stated reason (§14.1).
type AllowFact struct {
	Rule   string `json:"rule"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Reason string `json:"reason"`
}

// Key identifies a suppression.
func (a AllowFact) Key() string { return a.Rule + "|" + a.File }

// Dep is one Go module the app builds against (§9.6).
type Dep struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect,omitempty"`
}

// QueryFact is one query shape captured while tests ran (§11.4).
type QueryFact struct {
	Action string `json:"action"`
	SQL    string `json:"sql"`
	Table  string `json:"table"`
	Rows   int64  `json:"rows"`
	Count  int    `json:"count"`
}

// Sorted puts every collection in canonical order for hashing.
func (f *Facts) Sorted() {
	sort.Slice(f.Unsafe, func(i, j int) bool {
		if f.Unsafe[i].Module != f.Unsafe[j].Module {
			return f.Unsafe[i].Module < f.Unsafe[j].Module
		}
		return f.Unsafe[i].File+f.Unsafe[i].Reason < f.Unsafe[j].File+f.Unsafe[j].Reason
	})
	sort.Slice(f.Allows, func(i, j int) bool { return f.Allows[i].File < f.Allows[j].File })
	sort.Slice(f.Deps, func(i, j int) bool { return f.Deps[i].Path < f.Deps[j].Path })
	sort.Strings(f.Imports)
	sort.Strings(f.Covered)
	sort.Slice(f.Queries, func(i, j int) bool {
		if f.Queries[i].Action != f.Queries[j].Action {
			return f.Queries[i].Action < f.Queries[j].Action
		}
		return f.Queries[i].SQL < f.Queries[j].SQL
	})
}

// Bytes renders the canonical document.
func (f *Facts) Bytes() []byte {
	f.Sorted()
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		panic("model: facts: " + err.Error())
	}
	return append(b, '\n')
}
