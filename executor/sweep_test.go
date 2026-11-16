package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSweepRemovesContainersAndVolumesLabelledCanyonScan drives Sweep against the fake docker:
// it must ask for both by the canyon.scan label (contracts/executor.md invariant 8) and remove
// whatever it finds.
// Sweep removes every container labelled canyon.scan and its volumes, and every build's holder a crash left,
// labelled canyon.build (contracts/executor.md, A build reaches no private or link-local address).
func TestSweepRemovesContainersAndVolumesLabelledCanyonScan(t *testing.T) {
	e, dir := fakes(t)
	docker := `#!/bin/sh
printf '%s\n' "$@" >> "` + dir + `/argv"
case "$*" in
"ps -aq --filter label=canyon.scan") printf 'c1\nc2\n';;
"ps -aq --filter label=canyon.build") printf 'h1\n';;
"volume ls -q --filter label=canyon.scan") printf 'v1\n';;
esac
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	e.Sweep(context.Background())
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	for _, want := range []string{
		"ps\n-aq\n--filter\nlabel=canyon.scan\n",
		"rm\n-f\n-v\nc1\nc2\n",
		"ps\n-aq\n--filter\nlabel=canyon.build\n",
		"rm\n-f\n-v\nh1\n",
		"volume\nls\n-q\n--filter\nlabel=canyon.scan\n",
		"volume\nrm\nv1\n",
	} {
		if !strings.Contains(string(argv), want) {
			t.Errorf("sweep argv lacks %q: %q", want, argv)
		}
	}
}

// TestSweepFindsNothingRemovesNothing: a ps and a volume ls by label, and no rm at all when
// neither lists anything.
func TestSweepFindsNothingRemovesNothing(t *testing.T) {
	e, dir := fakes(t)
	docker := `#!/bin/sh
printf '%s\n' "$@" >> "` + dir + `/argv"
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	e.Sweep(context.Background())
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if !strings.Contains(string(argv), "ps\n-aq\n--filter\nlabel=canyon.scan\n") || !strings.Contains(string(argv), "volume\nls\n-q\n--filter\nlabel=canyon.scan\n") {
		t.Fatalf("sweep must ask by label: %q", argv)
	}
	if strings.Contains(string(argv), "\nrm\n") {
		t.Errorf("sweep removed something when nothing was labelled: %q", argv)
	}
}

// TestSweepLogsAndContinuesWhenRemovalFails: a failed docker rm is logged, never fatal — the
// volume sweep still runs after it.
func TestSweepLogsAndContinuesWhenRemovalFails(t *testing.T) {
	e, dir := fakes(t)
	docker := `#!/bin/sh
printf '%s\n' "$@" >> "` + dir + `/argv"
case "$*" in
"ps -aq --filter label=canyon.scan") printf 'c1\n';;
"rm -f -v c1") exit 1;;
"volume ls -q --filter label=canyon.scan") printf 'v1\n';;
esac
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	e.Sweep(context.Background())                                              // must not panic despite the failed rm
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))                         //nolint:gosec // test dir
	if !strings.Contains(string(argv), "volume\nrm\nv1\n") {
		t.Errorf("a failed container rm must not stop the volume sweep: %q", argv)
	}
}
