package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dockerWithImages is a docker for the cleanups, over files in dir: containers holds "name id" per
// container that exists (rm -f removes it), images "id repo:tag" per tag (every tag, whatever the reference filter asks:
// the executor filters the repository itself), volumes one name per volume. image rm untags, refusing a
// tag named in refuse; image ls waits while hold exists once a container was run (the cleanup's, never /run's own). A network is the file n/<name>, one member per
// line; create refuses a subnet listed in overlaps, and a name that exists (atomically, set -C), as
// docker does, and inspect is slow while slow exists and hangs on the network named in hang. Every call's argv lands in argv.
func dockerWithImages(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for _, f := range []string{"containers", "images", "volumes", "refuse", "overlaps"} {
		body := files[f]
		if f == "images" && body == "" { // the image runBody runs, so a /run finds it listed
			body = "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_0-run\n"
		}
		must(t, os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600))
	}
	must(t, os.MkdirAll(filepath.Join(dir, "n"), 0o700))
	script := `#!/bin/sh
d="` + dir + `"
` + hostAddrsFake + `printf '%s\n' "$@" >> "$d/argv"
case "$1 $2" in
"container ls") for a; do case "$a" in name=*) n=${a#name=^}; grep "^${n%?} " "$d/containers" | sed 's/ / running /';; esac; done;;
"run -d") : > "$d/ran"; echo "$4 sha256:abababababababababababababababababababababababababababababababab" >> "$d/containers";;
"container inspect") echo running 0 sha256:ab;;
"image ls") while [ -e "$d/hold" ] && [ -e "$d/ran" ]; do sleep 0.02; done; cat "$d/images";;
"image rm") grep -qx "$3" "$d/refuse" && { echo "conflict: $3"; exit 1; }; grep -v " $3\$" "$d/images" > "$d/i"; mv "$d/i" "$d/images";;
"volume inspect") shift 4; for n; do grep -x "$n" "$d/volumes"; done;;
"rm -f") shift 2; for n; do grep -v "^$n " "$d/containers" > "$d/c"; mv "$d/c" "$d/containers"; done;;
"volume rm") shift 2; for n; do grep -vx "$n" "$d/volumes" > "$d/v"; mv "$d/v" "$d/volumes"; done;;
"network inspect") [ -e "$d/slow" ] && sleep 0.2; [ "$5" = "$(cat "$d/hang" 2>/dev/null)" ] && exec sleep 30; [ -e "$d/n/$5" ] || exit 1
	printf '{"Name":"%s","Internal":false,"IPAM":{"Config":[{"Subnet":"10.9.8.0/24","Gateway":"10.9.8.1"}]},"Containers":{' "$5"; sep=
	for m in $(cat "$d/n/$5"); do printf '%s"id-%s":{"Name":"%s","IPv4Address":""}' "$sep" "$m" "$m"; sep=,; done; echo '}}';;
"network create") grep -qx "$5" "$d/overlaps" && { echo "Error response from daemon: invalid pool request: ` + overlaps + `"; exit 1; }
	grep -qx "$5" "$d/refuse" && { echo "Error response from daemon: refused"; exit 1; }; eval "net=\${$#}"; (set -C; : > "$d/n/$net") 2>/dev/null || exit 1;;
"network connect") echo "$5" >> "$d/n/$4";;
"network disconnect") grep -vx "$4" "$d/n/$3" > "$d/m"; mv "$d/m" "$d/n/$3";;
"network rm") rm "$d/n/$3";;
"network ls") ls "$d/n";;
esac
exit 0
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // test dir
	must(t, err)
	return string(b)
}

const runBody = `{"app":"claims-portal","target":"preview","image":"canyon/claims-portal@sha256:` + "abababababababababababababababababababababababababababababababab" + `","env":{},"network":"canyon-app-claims-portal"}`

// After a /run, a tag no container of the app runs goes; a tag whose image preview, prod or one set
// aside runs stays — both tags of an identical rebuild — and another app's repository is never touched.
func TestARunRemovesTheAppsImagesNoContainerRuns(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{
		"containers": "claims-portal-preview sha256:abababababababababababababababababababababababababababababababab\nclaims-portal-prod-previous sha256:bb\n",
		"images":     "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_1-a\nsha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_1-b\nsha256:bb canyon/claims-portal:pkg_2-c\nsha256:cc canyon/claims-portal:pkg_3-d\nsha256:cc canyon/claims-portal-other:pkg_4-e\n",
	})
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(runBody))
	must(t, err)
	_ = resp.Body.Close()
	e.collecting.Wait()
	if resp.StatusCode != 200 || read(t, dir, "images") != "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_1-a\nsha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_1-b\nsha256:bb canyon/claims-portal:pkg_2-c\nsha256:cc canyon/claims-portal-other:pkg_4-e\n" {
		t.Fatalf("%d; left %q", resp.StatusCode, read(t, dir, "images"))
	}
	if argv := read(t, dir, "argv"); strings.Count(argv, "image\nrm\n") != 1 || strings.Contains(argv, "-f\ncanyon/") || !strings.Contains(argv, "--filter\nname=^claims-portal-preview$\n--filter\nname=^claims-portal-prod$\n--filter\nname=^claims-portal-preview-previous$\n--filter\nname=^claims-portal-prod-previous$\n") {
		t.Fatalf("one plain image rm, after inspecting the four containers: %q", argv)
	}
}

// keep spares by digest — the call's own image, one named twice, one whose tags are not the server's
// to know — and a digest naming no image there is no error. A spared tag never counts against the 50,
// and /forget, which takes no keep, removes every image of the app all the same.
func TestCollectSparesKept(t *testing.T) {
	e, dir := fakes(t)
	kept, other, gone := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), "sha256:"+strings.Repeat("f", 64)
	running := "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_0-run\n" + kept + " canyon/claims-portal:pkg_1-a\n" + kept + " canyon/claims-portal:pkg_1-b\n"
	images := running
	for i := range maxCollect + 1 {
		images += fmt.Sprintf("%s canyon/claims-portal:pkg_2-%02d\n", other, i)
	}
	dockerWithImages(t, dir, map[string]string{"images": images + other + " canyon/claims-portal-other:pkg_3-c\n"})
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	body := strings.TrimSuffix(runBody, "}") + `,"keep":["` + kept + `","sha256:abababababababababababababababababababababababababababababababab","` + gone + `","` + kept + `"]}`
	resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(body))
	must(t, err)
	_ = resp.Body.Close()
	e.collecting.Wait()
	if want := running + other + " canyon/claims-portal:pkg_2-50\n" + other + " canyon/claims-portal-other:pkg_3-c\n"; resp.StatusCode != 200 || read(t, dir, "images") != want {
		t.Fatalf("%d; left %q, want %q", resp.StatusCode, read(t, dir, "images"), want)
	}
	must(t, os.WriteFile(filepath.Join(dir, "containers"), nil, 0o600))
	resp, err = srv.Client().Post(srv.URL+"/forget", "application/json", strings.NewReader(`{"app":"claims-portal","keep":["`+kept+`"]}`))
	must(t, err)
	_ = resp.Body.Close()
	if left := read(t, dir, "images"); resp.StatusCode != 204 || left != other+" canyon/claims-portal-other:pkg_3-c\n" {
		t.Fatalf("/forget with a keep: %d; left %q", resp.StatusCode, left)
	}
}

// keep: [] cleans up exactly as no keep.
func TestAnEmptyKeepIsNoKeep(t *testing.T) {
	var left []string
	for _, body := range []string{runBody, strings.TrimSuffix(runBody, "}") + `,"keep":[]}`} {
		e, dir := fakes(t)
		dockerWithImages(t, dir, map[string]string{"images": "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_0-run\nsha256:" + strings.Repeat("1", 64) + " canyon/claims-portal:pkg_1-a\n"})
		srv := httptest.NewServer(Handler(e))
		resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(body))
		must(t, err)
		_ = resp.Body.Close()
		e.collecting.Wait()
		srv.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", body, resp.StatusCode)
		}
		left = append(left, read(t, dir, "images"))
	}
	if left[0] != left[1] || strings.Contains(left[0], "pkg_1-a") {
		t.Fatalf("no keep left %q, keep: [] left %q", left[0], left[1])
	}
}

// A keep that is not a list of at most 16 digests is 400 {error}, answered before docker is asked
// anything; 16 with one twice is a keep, and [] is no keep.
func TestARunWithABadKeepIsRefusedBeforeAnything(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, nil)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	digest := "sha256:" + strings.Repeat("1", 64)
	var seventeen []string
	for i := range 17 {
		seventeen = append(seventeen, fmt.Sprintf(`"sha256:%064x"`, i))
	}
	sixteen := append(seventeen[:15:15], seventeen[0])
	post := func(keep string) (int, string) {
		resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(strings.TrimSuffix(runBody, "}")+`,"keep":`+keep+"}"))
		must(t, err)
		defer func() { _ = resp.Body.Close() }()
		var answer struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&answer)
		return resp.StatusCode, answer.Error
	}
	for _, keep := range []string{
		`["pkg_1-a"]`, // a tag moves
		`["canyon/claims-portal@` + digest + `"]`,
		`["` + strings.ToUpper(digest) + `"]`,
		`["` + digest[:70] + `"]`,
		`["` + digest + `0"]`,
		`"` + digest + `"`,
		`null`,
		`[1]`,
		"[" + strings.Join(seventeen, ",") + "]",
	} {
		if status, msg := post(keep); status != 400 || !strings.Contains(msg, "keep") {
			t.Errorf("keep %s: %d %q, want 400 naming keep", keep, status, msg)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "argv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("docker was asked before the refusal: %q", read(t, dir, "argv"))
	}
	for _, keep := range []string{"[" + strings.Join(sixteen, ",") + "]", "[]"} {
		if status, msg := post(keep); status != 200 {
			t.Errorf("keep %s: %d %q, want 200", keep, status, msg)
		}
	}
}

// The cleanup answers to nobody: /run has answered before it lists a single image, and a tag docker
// refuses to remove is kept and logged, never a failed /run.
func TestACleanupThatFailsOrHangsNeverReachesTheRun(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{"images": "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_5-f\nsha256:cc canyon/claims-portal:pkg_3-d\n", "refuse": "canyon/claims-portal:pkg_3-d\n"})
	must(t, os.WriteFile(filepath.Join(dir, "hold"), nil, 0o600))
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(runBody))
	must(t, err)
	_ = resp.Body.Close()
	must(t, os.Remove(filepath.Join(dir, "hold"))) // /run answered while image ls still waited
	e.collecting.Wait()
	if resp.StatusCode != 200 || read(t, dir, "images") != "sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_5-f\nsha256:cc canyon/claims-portal:pkg_3-d\n" {
		t.Fatalf("%d; left %q", resp.StatusCode, read(t, dir, "images"))
	}
}

// /forget refuses while any of the app's four containers exists; then it removes the app's images and
// its two cache volumes, exactly — another app whose name starts the same keeps both — and again is 204.
func TestForgetRemovesTheAppsImagesAndCachesOnceItsContainersAreGone(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{
		"containers": "claims-portal-prod-previous sha256:bb\n",
		"images":     "sha256:aa canyon/claims-portal:pkg_1-a\nsha256:aa canyon/claims-portal:pkg_1-b\nsha256:cc canyon/claims-portal-other:pkg_4-e\n",
		"volumes":    "canyon-cache-claims-portal-build\ncanyon-cache-claims-portal-other-build\ncanyon-cache-claims-portal-other-launch\n",
	})
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Forget(ctx, "claims-portal"); err == nil || !strings.Contains(err.Error(), "409") || strings.Contains(read(t, dir, "argv"), "\nrm\n") {
		t.Fatalf("a container set aside still exists: %v", err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "containers"), nil, 0o600))
	for range 2 {
		if err := c.Forget(ctx, "claims-portal"); err != nil {
			t.Fatal(err)
		}
	}
	if images, volumes := read(t, dir, "images"), read(t, dir, "volumes"); images != "sha256:cc canyon/claims-portal-other:pkg_4-e\n" || volumes != "canyon-cache-claims-portal-other-build\ncanyon-cache-claims-portal-other-launch\n" {
		t.Fatalf("left images %q volumes %q", images, volumes)
	}
	if err := e.Forget(ctx, "--all"); !errors.Is(err, ErrRequest) {
		t.Fatalf("a name the pattern refuses: %v", err)
	}
}

// A redeploy cut short leaves <app>-<target>-previous; /stop removes it with the container, so the
// delete's /forget that follows is not refused forever.
func TestStopRemovesTheContainerSetAsideSoForgetSucceeds(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{"containers": "claims-portal-prod-previous sha256:bb\n", "images": "sha256:bb canyon/claims-portal:pkg_2-c\n"})
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	for _, target := range []string{"preview", "prod"} {
		must(t, c.Stop(context.Background(), "claims-portal", target))
	}
	if err := c.Forget(context.Background(), "claims-portal"); err != nil || read(t, dir, "containers") != "" || read(t, dir, "images") != "" {
		t.Fatalf("forget after both stops: %v; containers %q images %q", err, read(t, dir, "containers"), read(t, dir, "images"))
	}
}

// A daemon that cannot list the app's containers is an error for /forget, never "none left": nothing of
// the app is removed while what runs is not known.
func TestForgetOnADaemonThatCannotListRemovesNothing(t *testing.T) {
	e, dir := fakes(t)
	dockerWithImages(t, dir, map[string]string{"images": "sha256:cc canyon/claims-portal:pkg_3-d\n", "volumes": "canyon-cache-claims-portal-build\n"})
	script := read(t, dir, "docker")
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(strings.Replace(script, "case \"$1 $2\" in\n", "case \"$1 $2\" in\n\"container ls\"|\"container inspect\") echo 'failed to connect to the docker API' >&2; exit 1;;\n", 1)), 0o700)) //nolint:gosec // test fake
	if err := e.Forget(t.Context(), "claims-portal"); err == nil {
		t.Fatal("a daemon that cannot list is an error")
	}
	if argv := read(t, dir, "argv"); strings.Contains(argv, "rm\n") {
		t.Fatalf("nothing removed: %q", argv)
	}
}

// pack writes each Windows-reserved name inside a volume name it is passed spelt out, falcon as falc_o_n
// (contracts/executor.md, /forget): the build passes the name pack creates, and /forget removes both.
func TestForgetRemovesTheCachesAsPackNamedThem(t *testing.T) {
	e, dir := fakes(t)
	if _, err := e.Build(context.Background(), BuildRequest{App: "smooth-falcon-lei", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
		t.Fatal(err)
	}
	if argv := read(t, dir, "pack-argv"); !strings.Contains(argv, "--cache\ntype=build;format=volume;name=canyon-cache-smooth-falc_o_n-lei-build\n--cache\ntype=launch;format=volume;name=canyon-cache-smooth-falc_o_n-lei-launch\n") {
		t.Errorf("the build names its caches otherwise than pack creates them: %q", argv)
	}
	dockerWithImages(t, dir, map[string]string{"volumes": "canyon-cache-smooth-falc_o_n-lei-build\ncanyon-cache-smooth-falc_o_n-lei-launch\n"})
	if err := e.Forget(t.Context(), "smooth-falcon-lei"); err != nil || read(t, dir, "volumes") != "" {
		t.Fatalf("forget: %v; left volumes %q", err, read(t, dir, "volumes"))
	}
}
