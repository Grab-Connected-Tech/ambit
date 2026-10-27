package diff

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MarshalJSON renders risk as a word, so a consumer can switch on it.
func (r Risk) MarshalJSON() ([]byte, error) { return []byte(`"` + r.Label() + `"`), nil }

// UnmarshalJSON accepts the word form.
func (r *Risk) UnmarshalJSON(b []byte) error {
	switch strings.Trim(string(b), `"`) {
	case "high":
		*r = High
	case "medium":
		*r = Medium
	default:
		*r = Low
	}
	return nil
}

// Format names an output shape (§15.3).
type Format string

const (
	Text     Format = "text"
	Markdown Format = "markdown"
	JSON     Format = "json"
)

// Render produces the report in the requested shape.
func Render(r Report, f Format) string {
	switch f {
	case JSON:
		return r.JSON()
	case Markdown:
		return r.Markdown()
	}
	return r.Text()
}

// JSON is the tooling form: a consumer switches on risk and category.
func (r Report) JSON() string {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Text is the terminal form, aligned in columns so a diff of a diff stays readable.
func (r Report) Text() string {
	if r.Empty() {
		return "no behavior change\n"
	}
	var b strings.Builder
	w := subjectWidth(r)
	for _, c := range r.Changes {
		mark := ""
		if c.Blocked {
			mark = "BLOCKED "
		}
		fmt.Fprintf(&b, "%-5s %-11s %-*s %s%s\n", c.Risk, c.Category, w, c.Subject, mark, c.Detail)
	}
	b.WriteString("\n" + r.Summary() + "\n")
	return b.String()
}

// Markdown is the pull-request form, with a table a reviewer can read top to bottom.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString("### Behavior diff\n\n")
	if r.Empty() {
		b.WriteString("No behavior change: the app can do exactly what it could before.\n")
		return b.String()
	}
	b.WriteString("| Risk | Area | What changed | Detail |\n| --- | --- | --- | --- |\n")
	for _, c := range r.Changes {
		mark := ""
		if c.Blocked {
			mark = "**blocked:** "
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` | %s%s |\n", c.Risk, c.Category, c.Subject, mark, c.Detail)
	}
	b.WriteString("\n" + r.Summary() + "\n")
	if r.Count(High) > 0 {
		b.WriteString("\n> High-risk changes require a code-owner approval.\n")
	}
	return b.String()
}

// Summary is the count line from §15.3.
func (r Report) Summary() string {
	return fmt.Sprintf("%d changes · %d high · %d medium · %d low · %d unsafe blocks · %d new dependencies",
		len(r.Changes), r.Count(High), r.Count(Medium), r.Count(Low), r.Unsafe, r.NewDeps)
}

func subjectWidth(r Report) int {
	w := 0
	for _, c := range r.Changes {
		if len(c.Subject) > w {
			w = len(c.Subject)
		}
	}
	if w > 48 {
		return 48
	}
	return w
}
