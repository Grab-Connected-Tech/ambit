package main

import (
	"flag"
	"testing"
)

func TestFlagsAcceptPositionalsAnywhere(t *testing.T) {
	var strict *bool
	fs, asJSON, err := flags("x", []string{"billing", "--json", "--strict", "more"}, func(fs *flag.FlagSet) {
		strict = fs.Bool("strict", false, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !asJSON || !*strict || fs.NArg() != 2 || fs.Arg(0) != "billing" || fs.Arg(1) != "more" {
		t.Fatalf("json=%v strict=%v args=%v", asJSON, *strict, fs.Args())
	}
}

func TestToolDiagsReadCompilerPositions(t *testing.T) {
	d := toolDiags("GO-BUILD", []byte("# keystone.dev/x\n./app/billing/actions.go:12:3: undefined: foo\nplain line\n"))
	if len(d) != 2 {
		t.Fatalf("got %d diagnostics: %+v", len(d), d)
	}
	if d[0].File != "app/billing/actions.go" || d[0].Line != 12 || d[0].Column != 3 || d[0].Message != "undefined: foo" {
		t.Fatalf("position not parsed: %+v", d[0])
	}
	if d[1].File != "" || d[1].Message != "plain line" {
		t.Fatalf("a line without a position keeps its text: %+v", d[1])
	}
}
