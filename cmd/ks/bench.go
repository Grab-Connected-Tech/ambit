package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var benchLine = regexp.MustCompile(`([0-9.]+) p95-ms`)

// cmdBench load-tests one action (or all) in process, through the full pipeline, and
// reports latency percentiles and allocations per call (§11.4). --max-p95 fails the run
// when the 95th percentile exceeds a budget, which is how CI catches a regression.
func cmdBench(args []string) error {
	var maxP95 *float64
	var count *string
	fs, _, err := flags("bench", args, func(fs *flag.FlagSet) {
		maxP95 = fs.Float64("max-p95", 0, "fail when p95 latency in ms exceeds this")
		count = fs.String("time", "2s", "how long to run each action (go test -benchtime)")
	})
	if err != nil {
		return err
	}
	pattern := "BenchmarkActions"
	if a := fs.Arg(0); a != "" {
		pattern += "/^" + regexp.QuoteMeta(a) + "$"
	}
	cmd := exec.Command("go", "test", "./gen", "-run", "^$", "-bench", pattern, "-benchmem", "-benchtime", *count)
	cmd.Env = os.Environ()
	if os.Getenv("KS_TEST_DB_URL") == "" {
		url, stop, err := disposablePostgres()
		if err != nil {
			return err
		}
		defer stop()
		cmd.Env = append(cmd.Env, "KS_TEST_DB_URL="+url)
	}
	out, err := cmd.CombinedOutput()
	os.Stdout.Write(out)
	if err != nil {
		return exitError{1}
	}
	if !strings.Contains(string(out), "BenchmarkActions/") {
		return fmt.Errorf("no benchmark ran; is %q an action? (ks gen writes gen/bench_test.go)", fs.Arg(0))
	}
	if *maxP95 > 0 {
		for _, m := range benchLine.FindAllStringSubmatch(string(out), -1) {
			if p95, _ := strconv.ParseFloat(m[1], 64); p95 > *maxP95 {
				return fmt.Errorf("p95 %.2fms is over the %.2fms budget", p95, *maxP95)
			}
		}
	}
	return nil
}
