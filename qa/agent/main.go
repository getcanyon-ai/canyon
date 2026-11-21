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
	"path/filepath"
	"regexp"
	"strings"
)

func main() {
	var err error
	switch {
	case len(os.Args) >= 2 && os.Args[1] == "run":
		err = run(os.Args[2:])
	case len(os.Args) == 3 && os.Args[1] == "score":
		err = score(os.Args[2])
	case len(os.Args) >= 2 && os.Args[1] == "consent":
		err = consent(os.Args[2:])
	default:
		err = fmt.Errorf("usage: %s", usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const usage = "agent run [--suffix r2] [--keep-apps] [--parallel N] [--hosted [label=]<dir>]… [--repo owner/name --push-key <dir>] " +
	"[--unseen-repo owner/name] [--fork-ref ref] <tester-home> <run-dir> <scenario.md>… | agent score <run-dir> | " +
	"agent consent <tester-home> <hosted-home> read|act"

// run is `agent run`: --suffix names this batch's apps qa-<scenario>-<suffix>, fresh on a host
// that already has qa-<scenario> from an earlier batch; --keep-apps leaves each app running.
func run(args []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	suffix := flags.String("suffix", "", "append -<suffix> to every app name")
	keepApps := flags.Bool("keep-apps", false, "leave each scenario's app running after its turns")
	parallel := flags.Int("parallel", 0, "scenarios at once; 0, every one")
	o := options{hosted: map[string]string{}, repos: map[string]string{}}
	flags.Func("hosted", "a hosted template HOME, [label=]dir, for scenarios with that home: line (none: the rest)", func(v string) error {
		label, dir, ok := strings.Cut(v, "=")
		if !ok {
			label, dir = "", v
		}
		if label != "" && !homeLabel.MatchString(label) {
			return fmt.Errorf("%q is no home label", label)
		}
		abs, err := filepath.Abs(dir)
		o.hosted[label] = abs
		return err
	})
	for flagName, p := range map[string]string{"repo": repoPlaceholder, "unseen-repo": unseenPlaceholder, "fork-ref": forkPlaceholder} {
		flags.Func(flagName, p+" in the scenarios", func(v string) error {
			if p != forkPlaceholder && !repoName.MatchString(v) {
				return fmt.Errorf("%q is no owner/name", v)
			}
			o.repos[p] = v
			return nil
		})
	}
	flags.Func("push-key", "a directory holding id_ed25519, a deploy key that writes to --repo alone, and known_hosts", func(v string) error {
		abs, err := filepath.Abs(v)
		o.pushKey = abs
		return err
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *parallel < 0 {
		return fmt.Errorf("--parallel %d: 0 or more", *parallel)
	}
	if flags.NArg() < 3 {
		return fmt.Errorf("usage: %s", usage)
	}
	if *suffix != "" && !regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`).MatchString(*suffix) {
		return fmt.Errorf("--suffix %q: lowercase letters, digits and inner dashes only, as an app name takes", *suffix)
	}
	o.suffix, o.keepApps, o.parallel = *suffix, *keepApps, *parallel
	return runAll(flags.Arg(0), flags.Arg(1), o, flags.Args()[2:])
}
