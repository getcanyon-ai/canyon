package answered

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// One GET with the address as Host and no credential, a redirect never followed, the body's first
// non-empty line; nothing answering is one of the contract's words, never the library's message.
func TestAskSaysWhatTheAppAnsweredInTheContractsWords(t *testing.T) {
	var host, cookie, auth string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, cookie, auth = r.Host, r.Header.Get("Cookie"), r.Header.Get("Authorization")
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", map[string]string{"/django": "text/html; charset=utf-8", "/text": "text/plain; charset=utf-8", "/json": "application/json"}[r.URL.Path])
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("\n  \n  <h1>Bad Request (400)</h1>  \n<p>more</p>" + strings.Repeat("x", 70<<10)))
	}))
	defer app.Close()
	if a := Ask(context.Background(), nil, app.URL+"/", "claims-portal.preview.localhost:7777"); a != (Answer{Status: 302}) || host != "claims-portal.preview.localhost:7777" || cookie != "" || auth != "" {
		t.Fatalf("a redirect: %+v, asked as %q with cookie %q and auth %q", a, host, cookie, auth)
	}
	// the body's line only when the answer says it is text or JSON: never a page's markup (#1740, the test cluster's <!DOCTYPE html>)
	for path, want := range map[string]Answer{"/django": {Status: 400}, "/text": {Status: 400, Said: "<h1>Bad Request (400)</h1>"}, "/json": {Status: 400, Said: "<h1>Bad Request (400)</h1>"}} {
		if a := Ask(context.Background(), nil, app.URL+path, "x"); a != want {
			t.Errorf("a 400 at %s: %+v, want %+v", path, a, want)
		}
	}
	garbage, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := garbage.Accept()
		if err == nil {
			_, _ = c.Write([]byte("SSH-2.0-not-http \x1b[2J secret-in-the-banner\r\n\r\n"))
			_ = c.Close()
		}
	}()
	refused := garbage.Addr().String()
	if a := Ask(context.Background(), nil, "http://"+refused+"/", "x"); a.Error != "not an HTTP answer" || strings.Contains(a.Said, "secret") {
		t.Fatalf("not HTTP: %+v", a)
	}
	_ = garbage.Close()
	if a := Ask(context.Background(), nil, "http://"+refused+"/", "x"); a != (Answer{Error: "refused"}) {
		t.Fatalf("refused: %+v", a)
	}
	for a, want := range map[Answer]string{{Status: 302}: `{"said":"","status":302}`, {Error: "refused"}: `{"error":"refused"}`, {Host: "h", Status: 200, Said: "ok"}: `{"host":"h","said":"ok","status":200}`} {
		if b, err := json.Marshal(a); err != nil || string(b) != want {
			t.Errorf("%+v: %s %v, want %s", a, b, err, want)
		}
	}
}

// An answer to say beside readiness is nothing answered, or a status outside 2xx and 3xx but 401 and 404
// (contracts/routes.md, envs.<env>.answered): the one rule the CLI and the promotions route read.
func TestFlaggedIsTheAnswerToSayBesideReadiness(t *testing.T) {
	for a, want := range map[Answer]bool{
		{Status: 200}: false, {Status: 302}: false, {Status: 399}: false, {Status: 401}: false, {Status: 404}: false,
		{Status: 199}: true, {Status: 400}: true, {Status: 403}: true, {Status: 500}: true, {Error: "refused"}: true,
	} {
		if got := a.Flagged(); got != want {
			t.Errorf("%+v flagged %v, want %v", a, got, want)
		}
	}
}

// The promotions route's refusal and the CLI's line are one sentence (contracts/routes.md, preview_answered).
func TestRefusedNamesTheVersionTheAnswerAndTheWayOut(t *testing.T) {
	const out = ". Fix the app and deploy again, or have a person promote it from the dashboard after checking it."
	for a, want := range map[Answer]string{
		{Status: 400, Said: "Bad Request (400)"}: "Not promoted: v1.4 answers 400 Bad Request at / on preview" + out,
		{Status: 599}:                            "Not promoted: v1.4 answers 599 at / on preview" + out,
		{Error: "no answer in 5 s"}:              "Not promoted: v1.4 gives no answer at / on preview (no answer in 5 s)" + out,
	} {
		if got := a.Refused("v1.4"); got != want {
			t.Errorf("%+v:\n got %q\nwant %q", a, got, want)
		}
	}
}
