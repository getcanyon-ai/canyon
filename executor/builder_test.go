package executor

import (
	"bytes"
	"cmp"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The server reads GET /builder's run_image for its plan alone (contracts/executor.md): "" from an executor
// that answers builder alone or no /builder at all, and an error for any other failure, never a run image.
func TestClientRunImageReadsGetBuilder(t *testing.T) {
	run := "registry.example/canyon-builder-run-image@sha256:" + "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
		fails      bool
	}{
		{"both", `{"builder":"sha256:b","run_image":"` + run + `"}`, 200, run, false},
		{"builder alone", `{"builder":"sha256:b"}`, 200, "", false},
		{"no /builder", ``, 404, "", false},
		{"cannot tell", `{"error":"the daemon did not answer"}`, 502, "", true},
		{"not JSON", `run`, 200, "", true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/builder" {
				t.Errorf("%s: %s %s", tc.name, r.Method, r.URL.Path)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		got, err := (&Client{Base: srv.URL}).RunImage(t.Context())
		srv.Close()
		if got != tc.want || (err != nil) != tc.fails {
			t.Errorf("%s: %q %v", tc.name, got, err)
		}
	}
}

// builderFake is a docker that holds no canyon-builder and builds one by running build, a shell command.
func builderFake(t *testing.T, build string) *Executor {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	must(t, os.WriteFile(docker, []byte("#!/bin/sh\n[ \"$1 $2\" = 'image inspect' ] && exit 1\n[ \"$1\" = build ] && { cat >/dev/null; "+build+"; exit 0; }\nexit 1\n"), 0o700)) //nolint:gosec // a test fake must be executable
	return &Executor{Docker: docker}
}

// The first deploy on a Docker host waits while the executor builds canyon-builder, and docker build --quiet
// prints nothing all that time: the executor's log says what it builds and what that costs, then once a minute
// that it is still at it, in whole minutes, and when it is done (#2697).
func TestTheBuilderSaysWhatItIsDoing(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	saved := stillEvery
	stillEvery = 20 * time.Millisecond // a minute, as the lines count it
	defer func() { stillEvery = saved }()
	builderImage.id = ""
	id, err := builderFake(t, "sleep 0.3; echo sha256:"+strings.Repeat("ab", 32)).BuilderImage(t.Context())
	if err != nil || id != "sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("BuilderImage: %q %v", id, err)
	}
	at, out := 0, logged.String()
	for _, want := range []string{
		"executor: building " + builderImage.repo + ", Canyon's build image (once per Docker host, and again when a Canyon release changes it): up to a 1.4 GB download, 3.3 GB on disk, about 7 minutes at 25 Mbit/s and longer on a slower line\n",
		"executor: still building " + builderImage.repo + ", 1 minute so far\n",
		"executor: still building " + builderImage.repo + ", 2 minutes so far\n",
		"executor: built " + builderImage.repo + " in ",
	} {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("the log does not say %q after what came before it:\n%s", want, out)
		}
		at += i + len(want)
	}
}

// canyon-builder's build has an hour, not the 10 minutes a database refresh or another image's build has: a
// slow line takes 10 minutes or more over it, and the first deploy fails when it is cut short. A build past its
// deadline still fails, as Canyon's own error saying what to do; and a build that fails says so in the log, which
// never just stops (#2697).
func TestTheBuilderGetsItsOwnDeadline(t *testing.T) {
	if got := cmp.Or(builderImage.deadline, dbDeadline); got != time.Hour {
		t.Fatalf("canyon-builder's build has %s, want an hour", got)
	}
	for _, other := range []*scanImage{runImage, scanTools["opengrep"].build} {
		if got := cmp.Or(other.deadline, dbDeadline); got != dbDeadline {
			t.Fatalf("%s's build has %s, want dbDeadline's %s", other.repo, got, dbDeadline)
		}
	}
	if said := (&pastDeadline{name: "canyon-builder", deadline: time.Hour}).Error(); said != "canyon-builder did not finish in 60 minutes, most likely a slow download. Deploy again to carry on: Docker keeps the layers it finished." {
		t.Fatalf("an hour's build past its deadline says %q", said)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	saved := builderImage.deadline
	builderImage.deadline = 300 * time.Millisecond
	defer func() { builderImage.deadline = saved }()
	builderImage.id = ""
	began := time.Now()
	_, err := builderFake(t, "exec sleep 30").BuilderImage(t.Context())
	said := (&pastDeadline{name: "canyon-builder", deadline: 300 * time.Millisecond}).Error()
	if took := time.Since(began); err == nil || took > 10*time.Second || err.Error() != said {
		t.Fatalf("a build past its deadline answered after %s: %v", took, err)
	}
	if want := "executor: " + builderImage.repo + " failed after 0 minutes: " + said + "\n"; !strings.Contains(logged.String(), want) {
		t.Fatalf("the log does not say %q:\n%s", want, logged.String())
	}
	builderImage.id = ""
	_, err = builderFake(t, "echo 'no space left on device' >&2; exit 1").BuilderImage(t.Context())
	if err == nil || !strings.HasPrefix(err.Error(), "canyon-builder: docker build "+builderImage.repo+": exit status 1") ||
		!strings.Contains(logged.String(), "executor: "+builderImage.repo+" failed after 0 minutes: docker build "+builderImage.repo+": exit status 1: \"\" no space left on device\n") {
		t.Fatalf("a build that failed: %v\n%s", err, logged.String())
	}
}

// The first deploy on a Docker host waits while GET /builder builds canyon-builder, before the build writes a line,
// so the build's own clock leaves it out (#2711): the answer says how long it waited, and the next one, the image
// there, says nothing of it. A call that waited behind another's build waited too.
func TestGetBuilderSaysHowLongItWaitedForCanyonsImages(t *testing.T) {
	dir := t.TempDir()
	id, run := "sha256:"+strings.Repeat("ab", 32), "sha256:"+strings.Repeat("cd", 32)
	docker := filepath.Join(dir, "docker")
	must(t, os.WriteFile(docker, []byte("#!/bin/sh\ncase \"$*\" in *canyon-run:*) echo "+run+"; exit 0;; esac\n"+ //nolint:gosec // a test fake must be executable
		"[ \"$1 $2\" = 'image inspect' ] && [ -e '"+dir+"/built' ] && { echo "+id+"; exit 0; }\n"+
		"[ \"$1\" = build ] && { cat >/dev/null; sleep 0.3; : > '"+dir+"/built'; echo "+id+"; exit 0; }\nexit 1\n"), 0o700))
	builderImage.id = ""
	srv := httptest.NewServer(Handler(&Executor{Docker: docker}))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	answers := make(chan time.Duration, 2)
	for range 2 { // two first deploys at once: one builds, the other waits behind it
		go func() {
			_, waited, err := c.Builder(t.Context())
			if err != nil {
				t.Error(err)
			}
			answers <- waited
		}()
	}
	for range 2 {
		if waited := <-answers; waited < 200*time.Millisecond || waited > 10*time.Second {
			t.Errorf("a first GET /builder waited %s for canyon-builder's 0.3 s build", waited)
		}
	}
	resp, err := srv.Client().Get(srv.URL + "/builder")
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	validates(t, schemaOf(t, "builder_result"), body.Bytes())
	if strings.Contains(body.String(), "waited_ms") {
		t.Errorf("with canyon-builder on the daemon GET /builder answered %s", body.Bytes())
	}
	if builder, waited, err := c.Builder(t.Context()); builder != id || waited != 0 || err != nil {
		t.Errorf("the image there: %q %s %v", builder, waited, err)
	}
}
