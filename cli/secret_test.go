package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// listFixture is the contract's golden for routes.md row 176, served here and by the server's own
// test (cmd/canyon-server/routes_secrets_test.go), so a field renamed on either side turns both red
// instead of leaving this suite green while the binary prints blanks.
//
// Its order is the server's — env, then name — and it is deliberately not name order, so a client
// that sorted the answer itself would move a row.
func listFixture(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/secrets/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func secrets(t *testing.T) []secretRow {
	t.Helper()
	var read secretsList
	if err := json.Unmarshal([]byte(listFixture(t)), &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Secrets) == 0 {
		t.Fatal("the contract golden lists no secrets, so nothing below renders a row")
	}
	return read.Secrets
}

func renderedSecrets(t *testing.T, rows []secretRow) string {
	t.Helper()
	var out strings.Builder
	(&app{out: &out}).renderSecrets("claims-portal", rows, at)
	return out.String()
}

// The whole rendering, byte for byte: the five columns, their widths, and the host's own order.
func TestSecretListRendersTheHostsListByteForByte(t *testing.T) {
	got := renderedSecrets(t, secrets(t))
	if want := golden(t, "secret-list.txt"); got != want {
		t.Errorf("canyon secret list rendered\n%s\ntestdata/secret-list.txt wants\n%s", got, want)
	}
}

// The server sorts by env then name and the CLI prints that order (routes_secrets.go): a client
// that re-sorts sorts one answer, and here name order would move every row.
func TestTheHostsOrderSurvivesTheRender(t *testing.T) {
	rows := secrets(t)
	want := make([]string, 0, len(rows)) // the expectation, taken before anything renders
	for _, row := range rows {
		want = append(want, cell(row.Name))
	}
	out, seen := renderedSecrets(t, rows), -1
	for _, name := range want {
		next := strings.Index(out, name)
		if next < 0 || next < seen {
			t.Errorf("%q is missing or out of the host's order in\n%s", name, out)
		}
		seen = next
	}
}

// No route reads a value back, so there is no value on this screen to print, mask or measure —
// and no VERSION or ORIGIN column, which the dashboard keeps and this list drops.
func TestNothingOnThisScreenClaimsToHaveSeenAValue(t *testing.T) {
	out := renderedSecrets(t, secrets(t))
	for _, unwanted := range []string{"VALUE", "VERSION", "ORIGIN", "•", "****"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("the list printed %q in\n%s", unwanted, out)
		}
	}
	if head := strings.Split(out, "\n")[3]; strings.TrimSpace(head) != "NAME               ENV      WHERE    SET BY                 SET" {
		t.Errorf("the columns are %q", head)
	}
}

// The org's data reaches a cell: a control byte in a name must not drive the terminal, whichever
// column it arrived in. The row is written here rather than taken from the contract golden, which
// carries only names a host may answer: the list renders what it was handed, not what it was
// supposed to be handed.
func TestANameCarryingAnEscapeByteCannotDriveTheTerminal(t *testing.T) {
	rows := []secretRow{{Name: "LEGACY\x1b[2mKEY", Env: "preview", Where: "build", Version: 1,
		SetBy: actorRef{Kind: "agent", Subject: "gate"}, SetAt: "2026-08-30T12:00:00Z", Origin: "user"}}
	out := renderedSecrets(t, rows)
	if strings.ContainsRune(out, 0x1b) || !strings.Contains(out, `\x1b`) {
		t.Errorf("the escape byte was printed raw or lost in\n%q", out)
	}
}

// A row Canyon made from a declared resource (origin provisioned) is made for each environment the
// app is deployed to, so it prints once, ENV "each env": printed as a preview row, an agent read the
// skill's "set for preview and not for production" into Canyon's own DATABASE_URL (QA batch
// 2026-10-07, express-postgres-fresh).
func TestARowCanyonMadePrintsOnceForEachEnv(t *testing.T) {
	canyon := actorRef{Kind: "canyon", Subject: "provisioner", Display: "Canyon"}
	dana := actorRef{Kind: "user", Subject: "sub-dana", Display: "Dana Whitfield"}
	for name, rows := range map[string][]secretRow{
		"preview alone": {{Name: "DATABASE_URL", Env: "preview", Where: "runtime", SetBy: canyon, SetAt: "2026-09-01T10:00:00Z", Origin: "provisioned"},
			{Name: "STRIPE_SECRET_KEY", Env: "preview", Where: "runtime", SetBy: dana, SetAt: "2026-09-01T10:00:00Z", Origin: "user"}},
		"both envs": {{Name: "DATABASE_URL", Env: "preview", Where: "runtime", SetBy: canyon, SetAt: "2026-09-01T10:00:00Z", Origin: "provisioned"},
			{Name: "STRIPE_SECRET_KEY", Env: "preview", Where: "runtime", SetBy: dana, SetAt: "2026-09-01T10:00:00Z", Origin: "user"},
			{Name: "DATABASE_URL", Env: "prod", Where: "runtime", SetBy: canyon, SetAt: "2026-09-01T11:00:00Z", Origin: "provisioned"}},
	} {
		got := renderedSecrets(t, rows)
		want := "\n  claims-portal\n\n  NAME               ENV       WHERE    SET BY          SET\n" +
			"  DATABASE_URL       each env  runtime  Canyon          1d ago\n" +
			"  STRIPE_SECRET_KEY  preview   runtime  Dana Whitfield  1d ago\n"
		if got != want {
			t.Errorf("%s rendered\n%s\nwant\n%s", name, got, want)
		}
	}
	// A person set the same name in prod: Canyon's row is not each env's, so every row prints as it is.
	got := renderedSecrets(t, []secretRow{{Name: "DATABASE_URL", Env: "preview", Where: "runtime", SetBy: canyon, SetAt: "2026-09-01T10:00:00Z", Origin: "provisioned"},
		{Name: "DATABASE_URL", Env: "prod", Where: "runtime", SetBy: dana, SetAt: "2026-09-01T10:00:00Z", Origin: "user"}})
	want := "\n  claims-portal\n\n  NAME          ENV      WHERE    SET BY          SET\n" +
		"  DATABASE_URL  preview  runtime  Canyon          1d ago\n" +
		"  DATABASE_URL  prod     runtime  Dana Whitfield  1d ago\n"
	if got != want {
		t.Errorf("a name set by hand too rendered\n%s\nwant\n%s", got, want)
	}
}

func TestAnEmptyListSaysWhatToTypeNext(t *testing.T) {
	got := renderedSecrets(t, nil)
	if want := golden(t, "secret-list-empty.txt"); got != want {
		t.Errorf("an empty list rendered\n%s\ntestdata/secret-list-empty.txt wants\n%s", got, want)
	}
}

// serveSecrets answers every request with body and records what it was asked for.
func serveSecrets(t *testing.T, status int, body string) (base string, asked *[]string) {
	t.Helper()
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI())
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL, seen
}

func inProject(t *testing.T, app string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \""+app+"\"\n")
	t.Chdir(dir)
}

// One read, at the route routes.md row 176 names, and exit 0: nothing here asks for the org or
// for a second opinion on an age.
func TestSecretListReadsOnceAndExitsZero(t *testing.T) {
	base, asked := serveSecrets(t, http.StatusOK, listFixture(t))
	inProject(t, "claims-portal")
	out, errs, code := run(t, map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}, false, "secret", "list")
	if code != exitOK || errs != "" {
		t.Errorf("canyon secret list: exit %d, stderr %q", code, errs)
	}
	want := []string{"GET /api/v1/projects/claims-portal/secrets"}
	if len(*asked) != 1 || (*asked)[0] != want[0] {
		t.Errorf("canyon secret list asked for %q, want %q", *asked, want)
	}
	if !strings.Contains(out, "claims-portal") || !strings.Contains(out, "STRIPE_SECRET_KEY") {
		t.Errorf("no header and no row in\n%s", out)
	}
}

// Any other refusal reaches the person in the host's own words.
func TestAHostsRefusalOfTheListPrintsTheHostsWords(t *testing.T) {
	base, _ := serveSecrets(t, http.StatusForbidden, `{"error":"forbidden","message":"only the app's team reads its secrets"}`)
	inProject(t, "claims-portal")
	out, errs, code := run(t, map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}, false, "secret", "list")
	if code != exitError || pastHeader(out) != "" || !strings.Contains(errs, "only the app's team reads its secrets") {
		t.Errorf("a refusal: exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// A canyon.toml with no app line is an app not made yet: set makes it, as a first deploy does, writes
// its id, and sets the value on it (cli.md §3.2; #1911).
func TestASecretSetWithNoAppLineMakesTheAppAndWritesItsId(t *testing.T) {
	var asked []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"project":{"id":"funny-snake-4k2"}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	dir := filepath.Join(t.TempDir(), "claims-app")
	writeFile(t, filepath.Join(dir, projectFile), "description = \"Claims\"\n")
	t.Chdir(dir)
	_, errs, code := answer(t, word+"\n", map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir()}, false, "secret", "set", "STRIPE_SECRET_KEY")
	body, _ := os.ReadFile(filepath.Join(dir, projectFile)) //nolint:gosec // the file this test wrote
	if code != exitOK || string(body) != "app = \"funny-snake-4k2\"\ndescription = \"Claims\"\n" ||
		strings.Join(asked, ",") != "POST /api/v1/projects,PUT /api/v1/projects/funny-snake-4k2/secrets/STRIPE_SECRET_KEY" {
		t.Errorf("exit %d, stderr %q, asked %q, file %q", code, errs, asked, body)
	}
}

// `canyon --help` prints one row per verb, so the three forms are listed here or nowhere. The one
// that is not built says so, and the word nobody recognises comes back on stderr.
func TestTheFormsArePrintedWhereverOneIsMissingOrUnknown(t *testing.T) {
	out, errs, code := run(t, nil, false, "secret")
	if code != exitOK || errs != "" {
		t.Errorf("canyon secret alone: exit %d, stderr %q", code, errs)
	}
	for _, want := range []string{"canyon secret list", "canyon secret set NAME      Store one value", "canyon secret rm NAME       Remove one name",
		"Any setting the app reads goes here, not only keys and passwords."} {
		if !strings.Contains(out, want) {
			t.Errorf("canyon secret alone did not print %q:\n%s", want, out)
		}
	}
	out, errs, code = run(t, nil, false, "secret", "bogus")
	if code != exitError || out != "" {
		t.Errorf("an unknown form: exit %d, stdout %q", code, out)
	}
	if !strings.Contains(errs, `Unknown secret command "bogus"`) || !strings.Contains(errs, "canyon secret list") {
		t.Errorf("an unknown form printed:\n%s", errs)
	}
	if _, errs, code := run(t, nil, false, "secret", "\x1b[2Jlist"); code != exitError || strings.ContainsRune(errs, 0x1b) {
		t.Errorf("a form carrying escape bytes: exit %d, stderr %q", code, errs)
	}
	if _, errs, code := run(t, nil, false, "secret", "list", "extra"); code != exitError || !strings.Contains(errs, "takes no arguments") {
		t.Errorf("an argument the list does not take: exit %d, stderr %q", code, errs)
	}
}

// removing runs `canyon secret rm` against a host that answers status and body, in a project
// directory, and hands back what the person saw and what the host was asked for.
func removing(t *testing.T, status int, body string, args ...string) (out, errs string, code int, asked *[]string) {
	t.Helper()
	base, seen := serveSecrets(t, status, body)
	inProject(t, "claims-portal")
	out, errs, code = run(t, map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}, false,
		append([]string{"secret", "rm"}, args...)...)
	return pastHeader(out), errs, code, seen
}

func TestSecretRmDeletesOneRowAndNamesWhatWent(t *testing.T) {
	out, errs, code, asked := removing(t, http.StatusNoContent, "", "STRIPE_SECRET_KEY")
	if code != exitOK || errs != "" {
		t.Errorf("canyon secret rm: exit %d, stderr %q", code, errs)
	}
	want := "DELETE /api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY?env=preview"
	if len(*asked) != 1 || (*asked)[0] != want {
		t.Errorf("canyon secret rm asked %q, want exactly one %q", *asked, want)
	}
	// The line says what was removed, from which app, and from which of the two environments.
	if want := "\n  ✓ Removed STRIPE_SECRET_KEY from claims-portal · preview\n"; out != want {
		t.Errorf("canyon secret rm printed %q, want %q", out, want)
	}
}

// The env the person typed is the env that reaches the query and the env the line names — the
// host is never left to default one, and an explicitly empty --env is the host's own default.
func TestTheEnvReachesTheQueryAndTheLine(t *testing.T) {
	for _, c := range []struct{ typed, sent string }{{"prod", "prod"}, {"", "preview"}} {
		out, _, code, asked := removing(t, http.StatusNoContent, "", "--env", c.typed, "DATABASE_URL")
		want := "DELETE /api/v1/projects/claims-portal/secrets/DATABASE_URL?env=" + c.sent
		if code != exitOK || len(*asked) != 1 || (*asked)[0] != want {
			t.Errorf("--env %q: exit %d, asked %q, want %q", c.typed, code, *asked, want)
		}
		if !strings.Contains(out, "· "+c.sent+"\n") {
			t.Errorf("--env %q printed\n%s", c.typed, out)
		}
	}
}

// A flag is read wherever it stands (canyonv2-sqvp): after the name as before it. An unknown flag
// after the name is still refused, never dropped unread, and "--" ends the flags.
func TestAFlagAfterTheNameIsRead(t *testing.T) {
	if _, errs, code, asked := removing(t, http.StatusNoContent, "", "DATABASE_URL", "--env", "prod"); code != exitOK || len(*asked) != 1 || (*asked)[0] != "DELETE /api/v1/projects/claims-portal/secrets/DATABASE_URL?env=prod" {
		t.Errorf("rm NAME --env prod: exit %d, asked %q, stderr %q", code, *asked, errs)
	}
	if _, errs, code, asked := removing(t, http.StatusNoContent, "", "DATABASE_URL", "--bogus"); code == exitOK || len(*asked) != 0 || !strings.Contains(errs, "bogus") {
		t.Errorf("rm NAME --bogus: exit %d, asked %q, stderr %q; want a refusal naming it", code, *asked, errs)
	}
	if _, _, code, asked := removing(t, http.StatusNoContent, "", "--", "--env"); code != exitOK || len(*asked) != 1 || !strings.Contains((*asked)[0], "/secrets/--env?env=preview") {
		t.Errorf("rm -- --env: exit %d, asked %q; want the name --env", code, *asked)
	}
}

// A name that is not there is the host's refusal, byte for byte: the CLI neither softens it nor
// guesses at a name it never validated.
func TestANameThatIsNotThereIsTheHostsRefusalUnchanged(t *testing.T) {
	out, errs, code, asked := removing(t, http.StatusNotFound, `{"error":"not_found","message":"no such secret"}`, "NOPE")
	if code != exitError || out != "" {
		t.Errorf("a name that is not there: exit %d, stdout %q", code, out)
	}
	if want := "\n  ✗ no such secret\n"; errs != want {
		t.Errorf("the host said %q and the CLI said %q", want, errs)
	}
	if len(*asked) != 1 {
		t.Errorf("a refused rm asked %q", *asked)
	}
}

// Nor is an env checked here: --env bogus is sent, and the host's own words come back.
func TestAnEnvTheHostRefusesIsStillSent(t *testing.T) {
	out, errs, code, asked := removing(t, http.StatusBadRequest,
		`{"error":"invalid_env","message":"env is preview or prod"}`, "--env", "bogus", "DATABASE_URL")
	if code != exitError || out != "" {
		t.Errorf("--env bogus: exit %d, stdout %q", code, out)
	}
	want := "DELETE /api/v1/projects/claims-portal/secrets/DATABASE_URL?env=bogus"
	if len(*asked) != 1 || (*asked)[0] != want {
		t.Errorf("--env bogus asked %q, want %q — no env rule lives in this binary", *asked, want)
	}
	if want := "\n  ✗ env is preview or prod\n"; errs != want {
		t.Errorf("the host said %q and the CLI said %q", want, errs)
	}
}

func TestSecretRmNamesExactlyOneSecret(t *testing.T) {
	for _, args := range [][]string{{}, {"DATABASE_URL", "STRIPE_SECRET_KEY"}} {
		out, errs, code, asked := removing(t, http.StatusNoContent, "", args...)
		if code != exitError || out != "" || !strings.Contains(errs, "takes one NAME") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, code, out, errs)
		}
		if len(*asked) != 0 {
			t.Errorf("%q reached the host: %q", args, *asked)
		}
	}
}

// rm asks nothing and takes no --yes: it names the row it removes, and the confirm flag is
// deploy --env prod's. On a terminal with nothing to read, a prompt would hang or take silence for
// consent; here the 204 is the whole interaction.
func TestSecretRmNeverAsksAndTakesNoYes(t *testing.T) {
	base, asked := serveSecrets(t, http.StatusNoContent, "")
	inProject(t, "claims-portal")
	env := map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}
	out, errs, code := answer(t, "", env, true, "secret", "rm", "DATABASE_URL")
	if code != exitOK || errs != "" || len(*asked) != 1 {
		t.Errorf("on a terminal with nothing to read: exit %d, stderr %q, asked %q", code, errs, *asked)
	}
	for _, question := range []string{"?", "[y/N]", "[Y/n]", "Enter"} {
		if strings.Contains(out, question) {
			t.Errorf("rm put a question (%q) to a terminal:\n%s", question, out)
		}
	}
	if _, errs, code := run(t, env, false, "secret", "rm", "--yes", "DATABASE_URL"); code != exitError || !strings.Contains(errs, "yes") {
		t.Errorf("--yes is deploy --env prod's: exit %d, stderr %q", code, errs)
	}
}

// A name is the user's, and the server validates none of it on this route: it must reach the host
// whole through the path and be spelled out on the way back to the screen.
func TestANameNeedingEscapingReachesTheHostWholeAndPrintsSpelledOut(t *testing.T) {
	out, _, code, asked := removing(t, http.StatusNoContent, "", "a/b\x1bc")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "a%2Fb%1Bc?env=preview") {
		t.Errorf("the host was asked %q", *asked)
	}
	if strings.ContainsRune(out, 0x1b) || !strings.Contains(out, `a/b\x1bc`) {
		t.Errorf("the line read %q", out)
	}
}

// word is what every test below sets: not a credential, and sharing no four-letter run with the CLI's own words.
const word = "pancake"

// quiet is the point of this verb: neither stream carries the value, whole or in part. Every substring of four bytes or more contains a four-gram.
func quiet(t *testing.T, where, out, errs, value string) {
	t.Helper()
	both := out + "\x00" + errs
	for i := 0; i+4 <= len(value); i++ {
		if strings.Contains(both, value[i:i+4]) {
			t.Fatalf("%s printed %q of the value:\n%s", where, value[i:i+4], both)
		}
	}
}

func settingHost(t *testing.T, status int, body string) (base string, asked *[]string, sent *[]secretPut) {
	t.Helper()
	seen, bodies := &[]string{}, &[]secretPut{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/projects/claims-portal" { // guard (a)'s read, before the value is asked for: the host has the app
			_, _ = w.Write([]byte(`{"project":{"id":"claims-portal"}}`))
			return
		}
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI())
		var in secretPut
		_ = json.NewDecoder(r.Body).Decode(&in)
		*bodies = append(*bodies, in)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL, seen, bodies
}

// setting is one canyon secret set, the value on stdin and nothing in argv, against a recording host.
func setting(t *testing.T, typed string, status int, body string, args ...string) (out, errs string, code int, asked *[]string, sent *[]secretPut) {
	t.Helper()
	base, seen, bodies := settingHost(t, status, body)
	inProject(t, "claims-portal")
	out, errs, code = answer(t, typed, map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}, false,
		append([]string{"secret", "set"}, args...)...)
	return pastHeader(out), errs, code, seen, bodies
}

func TestAPipedValueReachesTheHostWholeAndAppearsOnNoStream(t *testing.T) {
	out, errs, code, asked, sent := setting(t, word+"\n", http.StatusNoContent, "",
		"--env", "preview", "--where", "build", "STRIPE_SECRET_KEY")
	if want := "PUT /api/v1/projects/claims-portal/secrets/STRIPE_SECRET_KEY"; len(*asked) != 1 || (*asked)[0] != want {
		t.Fatalf("canyon secret set asked %q, want exactly one %q", *asked, want)
	}
	if len(*sent) != 1 || (*sent)[0] != (secretPut{Value: word, Env: "preview", Where: "build"}) {
		t.Errorf("the body was %+v", *sent)
	}
	want := "\n  \u2713 Set STRIPE_SECRET_KEY for claims-portal \u00b7 preview \u00b7 build\n" +
		"    Takes effect at the next deploy: canyon deploy."
	if code != exitOK || errs != "" || out != want+"\n" {
		t.Errorf("canyon secret set printed %q (exit %d, stderr %q), want %q", out, code, errs, want)
	}
	quiet(t, "a set that worked", out, errs, word)
}

// The line under a set names the command that makes it take effect: a deploy for preview, a promotion
// for production (#2873).
func TestASetSaysWhatMakesItTakeEffect(t *testing.T) {
	for env, want := range map[string]string{
		"preview": "    Takes effect at the next deploy: canyon deploy.\n",
		"prod":    "    Takes effect at the next promotion: canyon deploy --env prod.\n",
	} {
		out, _, code, _, _ := setting(t, word+"\n", http.StatusNoContent, "", "--env", env, "STRIPE_SECRET_KEY")
		if code != exitOK || !strings.HasSuffix(out, want) {
			t.Errorf("--env %s: exit %d, printed %q, want it to end %q", env, code, out, want)
		}
	}
}

func TestExactlyOneTrailingNewlineGoesAndNothingElseIsTouched(t *testing.T) {
	for _, c := range []struct{ typed, stored string }{
		{word + "\n", word},
		{word + "\r\n", word},
		{word, word},
		{word + "\n\n", word + "\n"},
		{word + "\r", word + "\r"},
		{"  " + word + "  ", "  " + word + "  "},
		{"-----BEGIN\nmiddle\n-----END\n", "-----BEGIN\nmiddle\n-----END"}, // a PEM key is the reason inner lines survive
	} {
		out, errs, code, _, sent := setting(t, c.typed, http.StatusNoContent, "", "DATABASE_URL")
		if code != exitOK || len(*sent) != 1 {
			t.Fatalf("%q: exit %d, stderr %q, bodies %+v", c.typed, code, errs, *sent)
		}
		if (*sent)[0].Value != c.stored {
			t.Errorf("%q was stored as %q, want %q", c.typed, (*sent)[0].Value, c.stored)
		}
		quiet(t, "a set of "+strconv.Quote(c.typed), out, errs, word)
	}
}

// The two rules this binary keeps for itself: an empty value, and the route's own cap. A value at the cap still goes, the shell's newline and all.
func TestTheTwoLocalRulesRefuseBeforeTheWire(t *testing.T) {
	for _, c := range []struct{ typed, want string }{
		{"", "an empty value is not a secret"},
		{"\n", "an empty value is not a secret"},
		{"\r\n", "an empty value is not a secret"},
		{strings.Repeat(word, maxValue/len(word)+1), "at most 65536 bytes"}, // a real value, so quiet below has something to find
	} {
		out, errs, code, asked, _ := setting(t, c.typed, http.StatusNoContent, "", "DATABASE_URL")
		if code != exitError || out != "" || !strings.Contains(errs, c.want) {
			t.Errorf("%d bytes: exit %d, stdout %q, stderr %q", len(c.typed), code, out, errs)
		}
		if len(*asked) != 0 {
			t.Errorf("%d bytes reached the host: %q", len(c.typed), *asked)
		}
		quiet(t, "a local refusal", out, errs, c.typed) // the cap's error is an error like any other
	}
	atTheCap := strings.Repeat("pancake", maxValue/7+1)[:maxValue] // a real value, so the no-leak rule has something to find
	out, errs, code, _, sent := setting(t, atTheCap+"\n", http.StatusNoContent, "", "DATABASE_URL")
	if code != exitOK || len(*sent) != 1 || len((*sent)[0].Value) != maxValue {
		t.Errorf("a value at the cap, with the shell's newline on it: exit %d, bodies %+d", code, len(*sent))
	}
	quiet(t, "a value at the cap", out, errs, atTheCap) // no exception: every test that sets a value checks both streams
}

// A harness can hold a terminal, and a question put to one hangs it for ever. /dev/zero is that
// terminal here: IsTTY reads a character device as one, and it never stops giving bytes, so a build
// missing this branch fails on the read cap in a moment instead of hanging the suite.
func TestATerminalNothingCanAnswerIsToldToPipeAndIsNeverRead(t *testing.T) {
	zero, err := os.Open("/dev/zero")
	if err != nil {
		t.Skip("no /dev/zero on this machine")
	}
	t.Cleanup(func() { _ = zero.Close() })
	base, asked, _ := settingHost(t, http.StatusNoContent, "")
	inProject(t, "claims-portal")
	env := map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()}
	var out, errs strings.Builder
	code := Main([]string{"--format", "ai", "secret", "set", "DATABASE_URL"}, &out, &errs, zero, func(k string) string { return env[k] }, true)
	if code != exitError || out.String() != "" {
		t.Errorf("a harness holding a terminal: exit %d, stdout %q", code, out.String())
	}
	if !strings.Contains(errs.String(), "printf %s VALUE | canyon secret set DATABASE_URL") {
		t.Errorf("the refusal did not name the pipe form:\n%s", errs.String())
	}
	if len(*asked) != 0 {
		t.Errorf("a refused set reached the host: %q", *asked)
	}
}

// Nothing offline clears a real echo bit, so this holds the build to the rule's other half: a terminal it cannot silence is refused, never asked.
func TestATerminalThatWillNotStopEchoingIsRefusedRatherThanAsked(t *testing.T) {
	base, asked, _ := settingHost(t, http.StatusNoContent, "")
	inProject(t, "claims-portal")
	out, errs, code := answer(t, word+"\n", map[string]string{"CANYON_HOST": base, "XDG_CONFIG_HOME": t.TempDir()},
		true, "secret", "set", "DATABASE_URL")
	if code != exitError || pastHeader(out) != "" || !strings.Contains(errs, "will not stop echoing") {
		t.Errorf("a terminal whose echo bit will not clear: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if len(*asked) != 0 {
		t.Errorf("a refused set reached the host: %q", *asked)
	}
	quiet(t, "the echo refusal", out, errs, word)
}

// What was typed is sent and what comes back is the host's: a rule repeated here can disagree with the one that counts, and a local PORT rule would never ask at all.
func TestWhatWasTypedIsSentAndWhatComesBackIsTheHosts(t *testing.T) {
	bad := `{"error":"invalid_name","message":"a secret name is letters, digits and underscores, not starting with a digit, at most 128 characters, and never PORT"}`
	for _, c := range []struct {
		args                 []string
		env, where, at, want string
	}{
		{[]string{"DATABASE_URL"}, "preview", "runtime", "/secrets/DATABASE_URL", ""},
		{[]string{"--env", "", "--where", "", "DATABASE_URL"}, "preview", "runtime", "/secrets/DATABASE_URL", ""},
		{[]string{"PORT"}, "preview", "runtime", "/secrets/PORT", "never PORT"},
		{[]string{"1ABC"}, "preview", "runtime", "/secrets/1ABC", "never PORT"},
		{[]string{"a/b"}, "preview", "runtime", "/secrets/a%2Fb", "never PORT"}, // a slash survives PathEscape: 400 invalid_name, not a route miss
		{[]string{"--env", "production", "--where", "runtime-ish", "X"}, "production", "runtime-ish", "/secrets/X", "never PORT"},
	} {
		status, body := http.StatusNoContent, ""
		if c.want != "" {
			status, body = http.StatusBadRequest, bad
		}
		out, errs, code, asked, sent := setting(t, word+"\n", status, body, c.args...)
		if len(*sent) != 1 || (*sent)[0].Env != c.env || (*sent)[0].Where != c.where {
			t.Errorf("%q sent %+v, want env %q where %q — neither rule lives in this binary", c.args, *sent, c.env, c.where)
		}
		if len(*asked) != 1 || !strings.HasSuffix((*asked)[0], c.at) {
			t.Errorf("%q asked %q, want one ending %q", c.args, *asked, c.at)
		}
		if c.want != "" && (code != exitError || out != "" || !strings.Contains(errs, c.want)) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", c.args, code, out, errs)
		}
		quiet(t, "a set of "+strings.Join(c.args, " "), out, errs, word)
	}
}

func TestSecretSetTakesOneNameAndNoValueBesideIt(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "takes one NAME"},
		{[]string{"DATABASE_URL", word}, "never given on the command line"},
	} {
		out, errs, code, asked, _ := setting(t, "", http.StatusNoContent, "", c.args...)
		if code != exitError || out != "" || !strings.Contains(errs, c.want) {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", c.args, code, out, errs)
		}
		if len(*asked) != 0 {
			t.Errorf("%q reached the host: %q", c.args, *asked)
		}
		quiet(t, "an argv refusal", out, errs, word)
	}
}

func TestThePromptTakesOneLineAndRefusesAPastedKey(t *testing.T) {
	for _, c := range []struct{ typed, read string }{
		{word + "\n", word + "\n"},
		{word, word},
		{"-----BEGIN\nmiddle\n-----END\n", ""}, // a pasted key, whole: its first line is not the value
	} {
		got, err := oneLine(bufio.NewReader(strings.NewReader(c.typed)))
		refused := err != nil && strings.Contains(err.Error(), "takes one line")
		if (c.read == "") != refused || (c.read != "" && got != c.read) {
			t.Errorf("%q read as %q with %v", c.typed, got, err)
		}
	}
}

// Plain http to anywhere but this machine is said once before a credential goes on the wire.
func TestPlainHttpToAnywhereButThisMachineIsSaidOnce(t *testing.T) {
	for host, warn := range map[string]bool{
		"http://localhost:7777": false, "http://127.0.0.1:7777": false, "http://[::1]:7777": false,
		"https://canyon.meridian.example": false, "http://canyon.internal": true, "http://10.1.2.3:7777": true,
	} {
		if clearNetwork(host) != warn {
			t.Errorf("%s: warned %v, want %v", host, !warn, warn)
		}
	}
	base, asked, _ := settingHost(t, http.StatusNoContent, "") // canyon.internal, dialled here: guard (a)'s read answers first
	inProject(t, "claims-portal")                              // the line is printed before the value is read, so nothing is sent
	client := newClient("http://canyon.internal")
	client.HTTP.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(base, "http://"))
	}}
	var errs strings.Builder
	a := &app{out: io.Discard, err: &errs, in: strings.NewReader(""), getenv: func(string) string { return "" }, clock: wallClock, http: client}
	if code := a.main([]string{"secret", "set", "DATABASE_URL"}); code != exitError || len(*asked) > 0 ||
		!strings.Contains(errs.String(), "canyon.internal is plain http, so this value crosses the network in clear text") {
		t.Errorf("a plain-http host: exit %d, asked %q, stderr %q", code, *asked, errs.String())
	}
}

// The gate's own fix text tells every agent to type this flag (contracts/testdata/finding/
// claims-portal.sarif.json, printed today in testdata/check-blocked.txt) and no route in v1 answers
// it (contracts/routes.md row 178), so every shape of it meets the same Canyon line, the parser is
// never the one that answers, and no value is read on the way there.
func TestFromGateIsANamedRefusalWhereverTheFlagLands(t *testing.T) {
	for _, args := range [][]string{
		{"STRIPE_SECRET_KEY", "--from-gate"},   // the fix text's own line, where no flag is read
		{"STRIPE_SECRET_KEY", "--from-gate=1"}, // the same place, carrying a value
		{"--from-gate", "STRIPE_SECRET_KEY"},
		{"--from-gate=1", "STRIPE_SECRET_KEY"},
		{"-from-gate", "STRIPE_SECRET_KEY"},
		{"--from-gate"},
		{"--env", "prod", "STRIPE_SECRET_KEY", "--from-gate"},
	} {
		out, errs, code, asked, _ := setting(t, word+"\n", http.StatusNoContent, "", args...)
		if code != exitError || out != "" {
			t.Errorf("%q: exit %d, stdout %q", args, code, out)
		}
		for _, want := range []string{
			"canyon secret set --from-gate is not available yet.",
			"nothing hands one over yet",
			"canyon secret set NAME",
		} {
			if !strings.Contains(errs, want) {
				t.Errorf("%q: stderr lost %q:\n%s", args, want, errs)
			}
		}
		if strings.Contains(errs, "fix text") { // no finding prints the flag since 5r11.21: the refusal says why it is refused, nothing more
			t.Errorf("%q: the refusal still blames a finding's fix text:\n%s", args, errs)
		}
		for _, parsers := range []string{"provided but not defined", "needs an argument", "came after an argument"} {
			if strings.Contains(errs, parsers) {
				t.Errorf("%q was answered in the parser's words, not Canyon's:\n%s", args, errs)
			}
		}
		if len(*asked) != 0 {
			t.Errorf("%q reached the host: %q", args, *asked)
		}
		quiet(t, "the --from-gate refusal", out, errs, word)
	}
}

// A refusal is not a suggestion. Nothing this CLI prints to say what to type next names the flag:
// not the forms block, not the unknown-subcommand block, not the help.
func TestFromGateIsSuggestedNowhere(t *testing.T) {
	for _, args := range [][]string{{"secret"}, {"secret", "bogus"}, {"help"}} {
		out, errs, _ := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, args...)
		if strings.Contains(out+errs, "from-gate") {
			t.Errorf("canyon %s offered --from-gate:\n%s%s", strings.Join(args, " "), out, errs)
		}
	}
	// Nor the verb's own --help: a flag that does nothing is no flag to list (#1990), and the docs
	// site's CLI reference is this help, published.
	out, errs, _ := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, "secret", "set", "--help")
	if strings.Contains(out+errs, "from-gate") {
		t.Errorf("the verb's help named --from-gate:\n%s%s", out, errs)
	}
}

// Asking how secret works is not an unknown command: --help prints the forms and exits 0 (audit §4, Help).
func TestSecretHelpPrintsTheFormsAndExitsZero(t *testing.T) {
	for _, ask := range []string{"--help", "-h", "help"} {
		out, errs, code := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, "secret", ask)
		if code != exitOK || !strings.Contains(out, "canyon secret list") || strings.Contains(errs, "Unknown secret command") {
			t.Errorf("secret %s: exit %d, stdout %q, stderr %q", ask, code, out, errs)
		}
	}
}

// A name a Docker host reserves (contracts/routes.md, the secrets PUT row; #2490) is said as the host says it, on
// stderr, exit 1, nothing on stdout and the value nowhere: the rule lives on the host, which knows its runtime.
func TestSecretSetSaysWhyADockerHostReservesAName(t *testing.T) {
	here, err := os.Getwd() // setting moves into the project's directory
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args         []string
		next, golden string
	}{
		{[]string{"DOCKER_USERNAME"}, "deploy", "testdata/secret-set-reserved.txt"},
		{[]string{"--where", "build", "HTTPS_PROXY"}, "build", "testdata/secret-set-reserved-build.txt"},
	} {
		name := c.args[len(c.args)-1]
		body := `{"error":"invalid_name","message":"` + name + ` is reserved here: this Canyon runs apps with Docker, and docker or pack reads it, so the app's next ` + c.next + ` would fail. Choose another name"}`
		want, err := os.ReadFile(filepath.Join(here, c.golden)) //nolint:gosec // a golden this test names
		if err != nil {
			t.Fatal(err)
		}
		out, errs, code, _, _ := setting(t, word+"\n", http.StatusBadRequest, body, c.args...)
		if code != exitError || out != "" || errs != string(want) {
			t.Errorf("%q: exit %d, stdout %q, stderr\n%s\n%s wants\n%s", c.args, code, out, errs, c.golden, want)
		}
		quiet(t, "a reserved name", out, errs, word)
	}
}

// Each form's --help names the form typed, not the verb it is dispatched under: `canyon secret rm --help`
// reads `canyon secret rm NAME [flags]`, as `canyon secret --help` writes it (#1990), never `canyon secret [flags]` (#888).
func TestASecretFormsHelpNamesTheForm(t *testing.T) {
	for form, want := range map[string]string{"list": "canyon secret list [flags]", "set": "canyon secret set NAME [flags]", "rm": "canyon secret rm NAME [flags]"} {
		out, errs, code := run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir()}, false, "secret", form, "--help")
		if code != exitOK || !strings.Contains(out, "\nUSAGE\n  "+want+"\n") {
			t.Errorf("secret %s --help: exit %d, stdout %q, stderr %q", form, code, out, errs)
		}
	}
}
