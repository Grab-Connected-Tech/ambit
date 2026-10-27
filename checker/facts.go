package checker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Grab-Connected-Tech/ambit/compile"
	"github.com/Grab-Connected-Tech/ambit/model"
)

// exprText renders an expression back to Go source, for the substring tests that
// decide which framework call a node represents.
func exprText(fset *token.FileSet, f *ast.File, n ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, n); err != nil {
		return ""
	}
	return buf.String()
}

var (
	hashRe    = regexp.MustCompile(`(?m)^// am:hash ([0-9a-f]{16})`)
	markerRe  = regexp.MustCompile(`(?m)^// am:([A-Za-z]+) `)
	gomodRe   = regexp.MustCompile(`^\s*([^\s]+)\s+(v[0-9][^\s/]*)`)
	declRe    = regexp.MustCompile(`^\s*(\w+)\s*=\s*s\.(Query|Mutation|Job|Schedule)\("(\w+)"`)
	implRe    = regexp.MustCompile(`^func\s+([A-Z]\w*)\s*\(`)
	requireRe = regexp.MustCompile(`env\.Call\([^,]+,\s*(actions\.)?([A-Z]\w*)`)
	// A test that wraps an action in its generated adapter (amtest.Call(env, actor,
	// gen.SendInvoiceHandler, ...), or a job handed to a worker) runs it.
	amtestCallRe = regexp.MustCompile(`gen\.([A-Z]\w*)Handler\b`)
)

// hashMarker returns the content hash recorded in a generated file.
func hashMarker(src string) string {
	m := hashRe.FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return m[1]
}

// hashMatches recomputes the hash of everything below the header comments.
func hashMatches(src, want string) bool {
	body := src
	for {
		i := strings.Index(body, "\n")
		if i < 0 || !strings.HasPrefix(body, "//") {
			break
		}
		body = body[i+1:]
	}
	// The package clause and imports are stable scaffolding; hash the declarations.
	sum := sha256.Sum256([]byte(normalizeForHash(body)))
	return hex.EncodeToString(sum[:])[:16] == want
}

// normalizeForHash drops whitespace differences so gofmt noise is not an edit.
func normalizeForHash(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		out = append(out, t)
	}
	return strings.Join(out, "\n")
}

// checkGeneratedHashes compares every generated file against what the model would
// produce now, which is how an edit to gen/ is caught (AM-CODE-001).
func checkGeneratedHashes(cfg Config, res *Result) {
	err := filepath.Walk(cfg.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".gen.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(src)
		if !strings.Contains(text, generatedMark) {
			res.Diags = append(res.Diags, compile.Diag{
				Rule: "AM-CODE-001", Severity: compile.SevError, File: rel(cfg.Root, path), Line: 1,
				Message: "a generated file is missing the generated marker",
				Fix:     "run am gen", Docs: "am explain AM-CODE-001",
			})
			return nil
		}
		want := hashMarker(text)
		if want == "" {
			return nil
		}
		if !hashMatches(text, want) {
			res.Diags = append(res.Diags, compile.Diag{
				Rule: "AM-CODE-001", Severity: compile.SevError, File: rel(cfg.Root, path), Line: 1,
				Message: "generated file was edited after am gen",
				Fix:     "put the change in the spec and run am gen, or move the work into actions/",
				Docs:    "am explain AM-CODE-001",
			})
		}
		return nil
	})
	_ = err
}

// checkImplementations reports declared actions with no function, and functions that
// no spec declares (§7.2: the entrypoint also fails the build, but this names the
// rule and points at the line).
func checkImplementations(cfg Config, res *Result) {
	if cfg.App == nil {
		return
	}
	impl := map[string]string{} // action name -> file
	for _, m := range cfg.App.Modules {
		for _, act := range m.Actions {
			if act.Kind == model.KindSchedule {
				continue // schedules are wired by the runtime, not written
			}
			dir := filepath.Join(cfg.Root, "app", m.Name, "actions")
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				src, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				for _, line := range strings.Split(string(src), "\n") {
					if mm := implRe.FindStringSubmatch(line); mm != nil {
						impl[mm[1]] = rel(cfg.Root, filepath.Join(dir, e.Name()))
					}
				}
			}
			if _, ok := impl[act.Name]; !ok {
				res.Diags = append(res.Diags, compile.Diag{
					Rule: "AM-CODE-005", Severity: compile.SevError, Decl: model.Qual(m.Name, act.Name),
					File: filepath.Join("app", m.Name, "actions"), Line: 1,
					Message: fmt.Sprintf("the spec declares %s but no action file implements it", act.Name),
					Fix:     "run am gen to write the stub, then fill it in",
					Docs:    "am explain AM-CODE-005",
				})
			}
		}
	}
}

// checkTests reports actions with no test that calls them (AM-TEST-001).
func checkTests(cfg Config, res *Result) {
	if cfg.App == nil {
		return
	}
	called := map[string]bool{}
	for _, m := range cfg.App.Modules {
		// Tests live beside the actions: in actions/, or in the module package itself.
		for _, dir := range []string{filepath.Join(cfg.Root, "app", m.Name, "actions"), filepath.Join(cfg.Root, "app", m.Name)} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !strings.HasSuffix(e.Name(), "_test.go") {
					continue
				}
				src, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					continue
				}
				for _, mm := range requireRe.FindAllStringSubmatch(string(src), -1) {
					called[mm[2]] = true
				}
				for _, mm := range amtestCallRe.FindAllStringSubmatch(string(src), -1) {
					called[mm[1]] = true
				}
			}
		}
	}
	for _, m := range cfg.App.Modules {
		for _, act := range m.Actions {
			if act.Kind == model.KindSchedule {
				continue
			}
			if !called[act.Name] {
				res.Diags = append(res.Diags, compile.Diag{
					Rule: "AM-TEST-001", Severity: compile.SevWarning, Decl: model.Qual(m.Name, act.Name),
					Message: fmt.Sprintf("%s has no test that calls it", model.Qual(m.Name, act.Name)),
					Fix:     "add env.Call(actor, actions." + act.Name + ", ...) in " + m.Name + "/actions",
					Docs:    "am explain AM-TEST-001",
				})
			}
		}
	}
}

// checkSuppressions turns //am:allow comments into suppression, and refuses the ones
// with no reason (§14.1).
func checkSuppressions(res *Result) {
	kept := res.Diags[:0]
	for _, a := range res.Facts.Allows {
		if strings.TrimSpace(a.Reason) == "" {
			res.Diags = append(res.Diags, compile.Diag{
				Rule: "AM-SEC-003", Severity: compile.SevError, File: a.File, Line: a.Line,
				Message: "//am:allow " + a.Rule + " has no reason",
				Fix:     "write why the rule does not apply here; the reason appears in the behavior diff",
				Docs:    "am explain AM-SEC-003",
			})
			continue
		}
	}
	suppressed := map[string]bool{}
	for _, a := range res.Facts.Allows {
		suppressed[a.Rule+"|"+a.File] = true
	}
	for _, d := range res.Diags {
		if d.Severity == compile.SevError && suppressed[d.Rule+"|"+d.File] {
			continue
		}
		kept = append(kept, d)
	}
	res.Diags = kept
}

// parseDeps reads require blocks out of go.mod, for the dependency rows of the diff.
func parseDeps(goMod string) []model.Dep {
	var out []model.Dep
	indirect := false
	for _, line := range strings.Split(goMod, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "require") && strings.Contains(t, "(") {
			indirect = true
			continue
		}
		if t == ")" {
			indirect = false
			continue
		}
		if strings.HasPrefix(t, "require ") {
			fields := strings.Fields(t)
			if len(fields) >= 3 {
				out = append(out, model.Dep{Path: fields[1], Version: fields[2], Indirect: strings.Contains(t, "// indirect")})
			}
			continue
		}
		m := gomodRe.FindStringSubmatch(t)
		if m == nil {
			continue
		}
		out = append(out, model.Dep{Path: m[1], Version: m[2], Indirect: indirect && strings.Contains(t, "// indirect")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// DeclaredActions scans spec files for action declarations, which lets am new and
// am context work without running the spec program.
func DeclaredActions(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(src), "\n") {
			if m := declRe.FindStringSubmatch(line); m != nil {
				out = append(out, moduleOf(path)+"."+m[3])
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

var _ = markerRe
