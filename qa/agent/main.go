// Command agent drives headless Claude Code, Codex or Cursor through Canyon scenarios and scores what it did.
//
//	go run ./qa/agent run <tester-home> <run-dir> qa/agent/scenarios/*.md
//	go run ./qa/agent score <run-dir>
//
// See qa/agent/README.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"regexp"
)

func main() {
	var err error
	switch {
	case len(os.Args) >= 2 && os.Args[1] == "run":
		err = run(os.Args[2:])
	case len(os.Args) == 3 && os.Args[1] == "score":
		err = score(os.Args[2])
	default:
		err = fmt.Errorf("usage: %s", usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const usage = "agent run [--suffix r2] [--keep-apps] <tester-home> <run-dir> <scenario.md>… | agent score <run-dir>"

// run is `agent run`: --suffix names this batch's apps qa-<scenario>-<suffix>, fresh on a host
// that already has qa-<scenario> from an earlier batch; --keep-apps leaves each app running.
func run(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	suffix := flags.String("suffix", "", "append -<suffix> to every app name")
	keepApps := flags.Bool("keep-apps", false, "leave each scenario's app running after its turns")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 3 {
		return fmt.Errorf("usage: %s", usage)
	}
	if *suffix != "" && !regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`).MatchString(*suffix) {
		return fmt.Errorf("--suffix %q: lowercase letters, digits and inner dashes only, as an app name takes", *suffix)
	}
	return runAll(flags.Arg(0), flags.Arg(1), *suffix, *keepApps, flags.Args()[2:])
}
