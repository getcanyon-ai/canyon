package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// fileOf is one access file the executor was handed, checked against the contract's access_file.
func fileOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	schema, err := jsonschema.NewCompiler().Compile("../../contracts/schema/executor.schema.json#/definitions/access_file")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err == nil {
		err = schema.Validate(doc)
	}
	var file map[string]any
	if err != nil || json.Unmarshal(raw, &file) != nil {
		t.Fatalf("the access file is not the contract's: %v\n%s", err, raw)
	}
	return file
}

// putFor is the last PUT /access the fake was handed for app and target.
func putFor(t *testing.T, app, target string) map[string]any {
	t.Helper()
	puts := fake.Accessed()
	for i := len(puts) - 1; i >= 0; i-- {
		if puts[i].App == app && puts[i].Target == target {
			return fileOf(t, puts[i].Access)
		}
	}
	t.Fatalf("no PUT /access for %s-%s among %d", app, target, len(puts))
	return nil
}

// grantees is each entry of an access file as kind:grantee:level.
func grantees(file map[string]any) []string {
	var out []string
	for _, e := range file["entries"].([]any) {
		m := e.(map[string]any)
		out = append(out, m["kind"].(string)+":"+m["grantee"].(string)+":"+m["level"].(string))
	}
	return out
}

// runOnFake starts image as app-target on the fake executor, as a deploy leaves it running.
func runOnFake(t *testing.T, app, target string) {
	t.Helper()
	if _, err := (&executor.Client{Base: fake.URL}).Run(context.Background(), executor.RunRequest{App: app, Target: target, Image: "canyon/" + app + "@" + executortest.Digest, Env: map[string]string{}, Network: "canyon-app-" + app}); err != nil {
		t.Fatal(err)
	}
}

// On Kubernetes a deploy's /run carries the app's access file and the front door, and the app's address is
// the front door's; each share and unshare re-PUTs the file wherever the app runs before it answers, and a
// PUT the executor fails answers 500.
func TestAKubernetesDeployCarriesTheAccessFileAndEveryShareRePUTsIt(t *testing.T) {
	withOIDC(t) // dana is the admin
	onKubernetes(t)
	cleanGate(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive) // olive owns it, carl contributes, vic reads
	door := `{` + doorOIDC + `,"domain":"canyon.meridian.example","form":"app.env.domain"}`
	if _, err := pool.Exec(ctx, `UPDATE packages SET roles = '{"approver": "Approves claims over 10,000 EUR"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO front_doors (network_policy_enforced_at, probed_at, door, set_by_kind, set_by_subject, set_by_display) VALUES (now(), now(), $1, 'person', 'sub-dana', 'Dana')", []byte(door)); err != nil {
		t.Fatal(err)
	}
	_, started, _ := do(t, olive, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
	if run := waitVerdict(t, olive, base, runOf(started)["id"].(string)); run["state"] != "clean" {
		t.Fatalf("the deploy: %s", lastBody)
	}
	ran := fake.Ran()
	sent := fileOf(t, ran.Access)
	want := map[string]any{"updated_at": sent["updated_at"], "groups_claim": "", "subject_issuer": "https://login.example.com/v2.0", "members": map[string]any{"domains": []any{"meridian.example"}, "emails": []any{}},
		"admins": map[string]any{"emails": []any{"dana@meridian.example"}, "claim": "", "value": ""}, "roles": map[string]any{"approver": "Approves claims over 10,000 EUR"},
		"entries": []any{
			map[string]any{"kind": "email", "grantee": "carl@meridian.example", "subject": "", "level": "contributor", "roles": []any{}},
			map[string]any{"kind": "email", "grantee": "olive@meridian.example", "subject": "sub-olive", "level": "owner", "roles": []any{}},
			map[string]any{"kind": "email", "grantee": "vic@meridian.example", "subject": "", "level": "viewer", "roles": []any{}}}}
	var sentDoor, wantDoor map[string]any
	_, _ = json.Unmarshal(ran.FrontDoor, &sentDoor), json.Unmarshal([]byte(door), &wantDoor)
	if !reflect.DeepEqual(sent, want) || !reflect.DeepEqual(sentDoor, wantDoor) {
		t.Fatalf("/run's access and front_door:\n%s\n%s", ran.Access, ran.FrontDoor)
	}
	if _, body, _ := do(t, olive, http.MethodGet, base+"/api/v1/projects/claims-portal", ""); body["project"].(map[string]any)["envs"].(map[string]any)["preview"].(map[string]any)["url"] != "https://claims-portal.preview.canyon.meridian.example" {
		t.Fatalf("the preview's address is the front door's: %s", lastBody)
	}
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/nora@meridian.example", `{"level":"viewer"}`); code != 200 {
		t.Fatalf("share: %d %s", code, lastBody)
	}
	shared := putFor(t, "claims-portal", "preview")
	if got := grantees(shared); !reflect.DeepEqual(got, []string{"email:carl@meridian.example:contributor", "email:nora@meridian.example:viewer", "email:olive@meridian.example:owner", "email:vic@meridian.example:viewer"}) ||
		shared["updated_at"].(string) <= sent["updated_at"].(string) {
		t.Fatalf("the share's file, newer than the deploy's: %v %v", got, shared["updated_at"])
	}
	putFor(t, "claims-portal", "prod") // asked too: a 404, nothing runs there
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"/email/vic@meridian.example", ""); code != 204 {
		t.Fatalf("unshare: %d %s", code, lastBody)
	}
	if got := grantees(putFor(t, "claims-portal", "preview")); !reflect.DeepEqual(got, []string{"email:carl@meridian.example:contributor", "email:nora@meridian.example:viewer", "email:olive@meridian.example:owner"}) {
		t.Fatalf("the unshare's file: %v", got)
	}
	fake.FailAccess.Store(true)
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/vic@meridian.example", `{"level":"viewer"}`); code != 500 || lastBody != `{"error":"internal","message":"internal error"}` {
		t.Fatalf("a share the executor could not take: %d %s", code, lastBody)
	}
}

// A preferences change to who is a member or an admin re-PUTs every running app's file before it answers;
// one to the stacks re-PUTs nothing.
func TestAPreferencesChangeRePUTsEveryRunningAppsAccessFile(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-portal'), ('ledger')"); err != nil {
		t.Fatal(err)
	}
	runOnFake(t, "claims-portal", "preview")
	runOnFake(t, "ledger", "prod")
	inSetup(t, ctx, pool) // only the admins may sign in, so they alone are the file's members
	if code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_emails":["lee@partner.example"]}`); code != 200 {
		t.Fatalf("the change in setup: %d %s", code, lastBody)
	}
	if members := putFor(t, "claims-portal", "preview")["members"]; !reflect.DeepEqual(members, map[string]any{"domains": []any{}, "emails": []any{"dana@meridian.example"}}) {
		t.Fatalf("the members in setup: %v", members)
	}
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET onboarded_at = now()"); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_emails":["kim@partner.example"]}`); code != 200 {
		t.Fatalf("the change: %d %s", code, lastBody)
	}
	if n := len(fake.Accessed()); n != 8 {
		t.Fatalf("both targets of both apps are asked each time: %d PUT(s)", n)
	}
	for _, app := range []struct{ id, target string }{{"claims-portal", "preview"}, {"ledger", "prod"}} {
		if emails := putFor(t, app.id, app.target)["members"].(map[string]any)["emails"]; !reflect.DeepEqual(emails, []any{"kim@partner.example"}) {
			t.Fatalf("%s-%s: the members the file names: %v", app.id, app.target, emails)
		}
	}
	if code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, stacksBody); code != 200 || len(fake.Accessed()) != 8 {
		t.Fatalf("the stacks: %d %s, %d PUT(s)", code, lastBody, len(fake.Accessed()))
	}
}

// A change to who may sign in answers only once every running app's file names it (the front door's B1), but it waits
// on the files together, not one after another: 20 apps on an executor that takes 200 ms a PUT answer within 2 s,
// where one at a time takes 8 (#2935).
func TestAPreferencesChangeWritesEveryAccessFileTogether(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	var apps []string
	for i := range 20 {
		apps = append(apps, fmt.Sprintf("app-%02d", i))
		if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ($1)", apps[i]); err != nil {
			t.Fatal(err)
		}
		runOnFake(t, apps[i], "preview")
	}
	fake.SlowAccess.Store(int64(200 * time.Millisecond))
	began := time.Now()
	code, _, _ := do(t, dana, http.MethodPut, base+preferencesPath, `{"allowed_emails":["kim@partner.example"]}`)
	if took := time.Since(began); code != 200 || took > 2*time.Second {
		t.Errorf("the change: %d after %s, want 200 within 2s: %s", code, took.Round(time.Millisecond), lastBody)
	}
	if n := len(fake.Accessed()); n != 40 {
		t.Errorf("%d PUT(s), want both targets of each of the 20 apps", n)
	}
	for _, app := range apps {
		if emails := putFor(t, app, "preview")["members"].(map[string]any)["emails"]; !reflect.DeepEqual(emails, []any{"kim@partner.example"}) {
			t.Errorf("%s: the members the file names when the change answered: %v", app, emails)
		}
	}
}

// On Docker there is no access file: a share re-PUTs nothing.
func TestADockerHostRePUTsNoAccessFile(t *testing.T) {
	_, _, base, olive, _, _, _, _ := teamOf(t)
	if code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/nora@meridian.example", `{"level":"viewer"}`); code != 200 || len(fake.Accessed()) != 0 {
		t.Fatalf("a Docker host's share: %d %s, %d PUT(s)", code, lastBody, len(fake.Accessed()))
	}
}

// waitPut waits for a PUT /access to claims-portal-preview at index from or later whose grantees meet want.
func waitPut(t *testing.T, from int, want func([]string) bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		puts := fake.Accessed()
		for _, put := range puts[min(from, len(puts)):] {
			if put.App == "claims-portal" && put.Target == "preview" && want(grantees(fileOf(t, put.Access))) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no such PUT /access among %d after %d", len(puts), from)
		}
	}
}

// An unshare whose re-PUT failed answers 500, and the reconcile's next tick takes the grant off the running
// app: the reconcile keeps going after the start's pass, so nothing that failed has to be remembered.
func TestAFailedUnshareLeavesTheRunningAppWithinOneReconcileTick(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive) // vic reads it
	runOnFake(t, "claims-portal", "preview")
	tick, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var logs syncBuffer
	go func() {
		(&app{pool: pool, pipe: pipe, kube: true, oidc: testOIDC}).reconcileAccess(tick, log.New(&logs, "", 0), 50*time.Millisecond)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	vic := func(g []string) bool { return slices.Contains(g, "email:vic@meridian.example:viewer") }
	waitPut(t, 0, vic) // the start's pass
	fake.FailAccess.Store(true)
	if code, _, _ := do(t, olive, http.MethodDelete, base+accessAt+"/email/vic@meridian.example", ""); code != 500 {
		t.Fatalf("an unshare the executor could not take: %d %s", code, lastBody)
	}
	fake.FailAccess.Store(false)
	waitPut(t, len(fake.Accessed()), func(g []string) bool { return !vic(g) }) // a PUT the executor took, without vic
	if !strings.HasPrefix(logs.String(), "canyon-server: access files: 1 written, 1 not running\n") {
		t.Fatalf("the reconcile's log, one line a pass:\n%s", logs.String())
	}
}

// A claim re-PUTs the file as a share does: the running app's file names the new owner.
func TestAClaimRePUTsTheAccessFile(t *testing.T) {
	withOIDC(t) // dana is the admin
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedTeamApp(t, ctx, pool, base, signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false))
	runOnFake(t, "claims-portal", "preview")
	dana := signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/projects/claims-portal/owner", `{"display":"Dana Whitfield"}`); code != 200 {
		t.Fatalf("the claim: %d %s", code, lastBody)
	}
	if got := grantees(putFor(t, "claims-portal", "preview")); !slices.Contains(got, "email:dana@meridian.example:owner") {
		t.Fatalf("the claim's file: %v", got)
	}
}

// A PUT /access the executor holds holds up nothing but itself: while a share's is held, a deploy's access
// file is still built.
func TestAHeldAccessPutHoldsUpNoDeploy(t *testing.T) {
	withOIDC(t)
	onKubernetes(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	runOnFake(t, "claims-portal", "preview")
	hold := make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release) // before the fake's own: it closes only once its handlers return
	fake.HoldAccess.Store(&hold)
	shared := make(chan int, 1)
	go func() {
		code, _, _ := do(t, olive, http.MethodPut, base+accessAt+"/email/nora@meridian.example", `{"level":"viewer"}`)
		shared <- code
	}()
	waitPut(t, 0, func([]string) bool { return true }) // the share's PUT is in, and held
	built := make(chan error, 1)
	go func() { _, err := pipe.Access(ctx, "claims-portal"); built <- err }()
	select {
	case err := <-built:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a deploy's access file waits on a held PUT /access")
	}
	release()
	if code := <-shared; code != 200 {
		t.Fatalf("the share: %d", code)
	}
}

// A grant bound under another issuer than Canyon's sign-in's goes in the access file unbound, its subject "":
// the sidecar matches it by address, never by a subject another issuer issued. One bound by a binary that
// recorded no issuer is Canyon's sign-in's, as that binary read it (#2306).
func TestTheAccessFileCarriesAGrantBoundUnderAnotherIssuerUnbound(t *testing.T) {
	pool, ctx := migrated(t)
	const canyon = "https://login.example.com/v2.0"
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-portal')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO project_access (project_id, kind, grantee, level, subject, subject_issuer, granted_by_kind, granted_by_subject, granted_by_display) VALUES
		('claims-portal', 'email', 'anna@meridian.example', 'owner', 'sub-anna', $1, 'person', 'sub-dana', ''),
		('claims-portal', 'email', 'ivo@meridian.example', 'viewer', 'sub-ivo', 'https://login.contoso.example', 'person', 'sub-dana', ''),
		('claims-portal', 'email', 'lea@meridian.example', 'viewer', 'sub-lea', '', 'person', 'sub-dana', ''),
		('claims-portal', 'email', 'nils@meridian.example', 'viewer', 'sub-nils', 'none', 'person', 'sub-dana', '')`, canyon); err != nil {
		t.Fatal(err)
	}
	a := &app{pool: pool, oidc: &oidcConfig{issuer: canyon}}
	raw, err := a.accessFile(ctx, org{domains: []string{"meridian.example"}, emails: []string{}, admins: []string{}}, "claims-portal", "2026-10-17T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	file := fileOf(t, raw)
	subjects := map[string]any{}
	for _, e := range file["entries"].([]any) {
		subjects[e.(map[string]any)["grantee"].(string)] = e.(map[string]any)["subject"]
	}
	if want := map[string]any{"anna@meridian.example": "sub-anna", "ivo@meridian.example": "", "lea@meridian.example": "sub-lea", "nils@meridian.example": ""}; file["subject_issuer"] != canyon || !reflect.DeepEqual(subjects, want) {
		t.Fatalf("the file's subjects under %v: %v, want %v", file["subject_issuer"], subjects, want)
	}
}
