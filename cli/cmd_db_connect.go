package cli

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// dbFirstPort is where a host, app and env with no remembered port starts looking (design/DATABASE-SESSIONS.md §9).
const dbFirstPort = 54329

// dbPortTries is how many ports past the remembered one a taken port tries before giving up.
const dbPortTries = 100

// dbLocal is what this machine keeps for one host, app and env: the local token a client's password is checked
// against, and the port last listened on, so a saved client connection keeps working across runs (§9).
type dbLocal struct {
	Token string `json:"token"`
	Port  int    `json:"port"`
}

// dbLocalPath is that file beside config.json, under db/; "" for a host that gets no file (hostFile), whose token
// and port then last one run.
func dbLocalPath(getenv func(string) string, host, app, env string) string {
	name := hostFile(host)
	path, err := configPath(getenv)
	if name == "" || err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(path), "db", strings.TrimSuffix(name, ".json")+"_"+app+"_"+env+".json")
}

// loadDBLocal reads that file; a missing or unreadable one, a token that is not 40 hex characters or a port outside
// 1024–65535 is replaced, never trusted. fresh replaces the token whatever it reads (--new-token, #2841).
func loadDBLocal(path string, fresh bool) (dbLocal, error) {
	var l dbLocal
	if path != "" {
		if body, err := os.ReadFile(path); err == nil { //nolint:gosec // this CLI's own file, under its config directory
			_ = json.Unmarshal(body, &l)
		}
	}
	if _, err := hex.DecodeString(l.Token); fresh || err != nil || len(l.Token) != 40 {
		b := make([]byte, 20)
		if _, err := rand.Read(b); err != nil {
			return l, err
		}
		l.Token = hex.EncodeToString(b)
	}
	if l.Port < 1024 || l.Port > 65535 {
		l.Port = 0
	}
	return l, nil
}

// signInToken is the token a client signs in with now: the one saved in the file, read at each sign-in, so a
// --new-token in another run reaches this one at once (#2841's review); the run's own when it has no file. A file
// that no longer reads back as a token signs nobody in.
func (l *dbListener) signInToken() (string, bool) {
	if l.path == "" {
		return l.token, true
	}
	var saved dbLocal
	body, err := os.ReadFile(l.path) //nolint:gosec // this CLI's own file, under its config directory
	if err != nil || json.Unmarshal(body, &saved) != nil {
		return "", false
	}
	if _, err := hex.DecodeString(saved.Token); err != nil || len(saved.Token) != 40 {
		return "", false
	}
	return saved.Token, true
}

func saveDBLocal(path string, l dbLocal) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return atomicWrite(path, body, true)
}

// listenDB listens on loopback at the remembered port, or the next free one after it.
func listenDB(want int) (net.Listener, int, error) {
	start := cmp.Or(want, dbFirstPort)
	for port := start; port < start+dbPortTries && port <= 65535; port++ {
		if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("no port is free on 127.0.0.1 from %d to %d", start, min(start+dbPortTries-1, 65535))
}

// dbOpened is the open's answer: the database session read, and that read as it came, for json.
type dbOpened struct {
	session dbSession
	raw     json.RawMessage
}

// runDBConnect opens the person's database session and listens on loopback for Postgres clients, one relayed
// connection each, until Ctrl-C or the database session ends (design/DATABASE-SESSIONS.md §3, §9).
func runDBConnect(ctx context.Context, a *app, args []string) int {
	var env, reason, resource, named string
	var write, replace, newToken bool
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&env, "env", "preview", "preview | prod")
		fs.StringVar(&reason, "reason", "", "Why you are opening production's data, in your words")
		fs.BoolVar(&write, "write", false, "Open a read-write session: statements may change data")
		fs.StringVar(&resource, "resource", "", "Which database, when the app declares more than one")
		fs.StringVar(&named, "app", "", "The app to connect to, when it is not the one "+projectFile+" names")
		fs.BoolVar(&replace, "replace", false, "Close your open session of the other mode and open this one")
		fs.BoolVar(&newToken, "new-token", false, "Replace the URL's token with a new one: the old one stops working")
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("db connect takes no arguments, and got %q", rest[0]))
	}
	if env != "preview" && env != "prod" {
		return a.usage(fmt.Errorf("--env is preview or prod, and got %q", env))
	}
	app, err := dbApp(named)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	mode := map[bool]string{false: "read", true: "write"}[write]
	body := map[string]any{"env": env, "mode": mode}
	if reason != "" {
		body["reason"] = reason
	}
	if resource != "" {
		body["resource"] = resource
	}
	if replace {
		body["replace"] = true
	}
	var opened dbOpened
	err = client.postJSON(ctx, dbOpen+url.PathEscape(app)+dbSessionsPath, body, statusOpened, &opened.raw)
	if err == nil {
		err = json.Unmarshal(opened.raw, &opened.session)
	}
	if err != nil {
		if a.render.Format == FormatAI {
			return a.dbRefusedAI("CONNECT", app, env, mode, err)
		}
		return a.fail(err)
	}
	path := dbLocalPath(a.getenv, client.Base, app, env)
	local, err := loadDBLocal(path, newToken)
	if err != nil {
		return a.fail(err)
	}
	ln, port, err := listenDB(local.Port)
	if err != nil {
		return a.fail(err)
	}
	defer func() { _ = ln.Close() }()
	moved := local.Port != 0 && port != local.Port
	was := local.Port
	if !moved { // a taken port is this run's alone: the remembered one stays, for the saved connection (#2841's review)
		local.Port = port
	}
	if err := saveDBLocal(path, local); err != nil {
		return a.fail(err)
	}
	cert, err := loopbackCert()
	if err != nil {
		return a.fail(err)
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	l := &dbListener{a: a, client: client, session: opened.session, app: app, token: local.Token, path: path, cert: cert, ended: make(chan string, 1), poll: dbPoll}
	s := opened.session
	who := s.Person.Email
	if name, _, found := strings.Cut(who, "@"); found {
		who = name // the address's local part: the user a client sends, which the listener ignores
	}
	dsn := (&url.URL{Scheme: "postgres", User: url.UserPassword(cmp.Or(who, "canyon"), local.Token), Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path: "/" + app, RawQuery: "sslmode=require&require_auth=scram-sha-256"}).String()
	if code, done := l.opened(opened, dsn, port, was, moved, newToken); done {
		return code
	}
	began := time.Now()
	l.live.Add(1) // serve itself, so stop waits until it has stopped taking clients
	go func() {
		defer l.live.Done()
		l.serve(ctx, ln)
	}()
	go l.watch(ctx)
	select {
	case <-ctx.Done():
	case reason := <-l.ended:
		l.stop(ln) // every connection's last line before the run's
		l.say(true, "\n%s %s\n", a.render.glyph(stateFail), l.endLine(reason))
		return exitError
	}
	l.stop(ln)
	_ = client.del(context.WithoutCancel(ctx), "/api/v1/db-sessions/"+url.PathEscape(s.ID)+"?if_idle=true") // in use or gone, it stays the host's to end
	minutes := int(time.Since(began).Minutes())
	l.say(false, "%s Closed %s %d min %s %s %s %s from the database\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), a.dot(), minutes, a.dot(),
		plural(int(l.count.Load()), "connection", "connections"), a.dot(), size(l.fromDB.Load()))
	return exitOK
}

// opened says the database session is open and where to connect, in the format asked; done is true when there is
// nothing more to say because the format is json's one line and the listener still runs, false otherwise.
func (l *dbListener) opened(o dbOpened, dsn string, port, was int, moved, renewed bool) (int, bool) {
	a, s, dot := l.a, o.session, " "+l.a.dot()+" "
	switch a.render.Format {
	case FormatJSON:
		var read map[string]json.RawMessage
		if err := json.Unmarshal(o.raw, &read); err != nil {
			return a.fail(err), true
		}
		read["url"], _ = json.Marshal(dsn)
		a.json(read)
	case FormatAI:
		a.printf("CANYON DB CONNECT — RESULT: OPEN\n\napp: %s\nenv: %s\nmode: %s\nas: %s\nurl: %s\nexpires: %s\n", cell(l.app), cell(s.Env), dbModeWord(s.Mode), dbWho(s), dsn, cell(s.ExpiresAt))
		if renewed {
			a.printf("token: new\n") // so an agent tells the person to update their saved connection (#2841's review)
		}
		a.printf("outcome: a %s database session is open; the url works only while this command runs.\n\n%s\nCANYON DB CONNECT — RESULT: OPEN\n", dbModeWord(s.Mode), dbConnectSteps)
	default:
		a.printf("  %s\n\n", a.render.paint(ansiDim, cell(l.app)+dot+dbEnvWord[s.Env]+dot+dbModeWord(s.Mode)+dot+"as "+dbWho(s)))
		if moved {
			a.printf("%s Port %d is taken: listening on %d for this run only.\n", a.render.glyph(statePending), was, port)
		}
		if renewed {
			a.printf("%s New token: update the password in your saved connection.\n", a.render.glyph(statePending))
		}
		until := ""
		if at, err := time.Parse(time.RFC3339, s.ExpiresAt); err == nil {
			until = dot + "until " + at.Local().Format("15:04") + ", or " + strconv.Itoa(s.IdleTimeout/60) + " min idle"
		}
		a.printf("%s Listening on 127.0.0.1:%d%s\n  %s\n\n  This database session is recorded as you. Ctrl-C stops listening.\n\n",
			a.render.paint(ansiGreen, a.render.glyph(statePass)), port, until, dsn)
	}
	return exitOK, false
}

// dbConnectSteps are the ai block's NEXT STEPS (design/DATABASE-SESSIONS.md §9).
const dbConnectSteps = `NEXT STEPS:
1. For single statements, prefer canyon db query.
2. If a client must connect, run this command in the background and stop it when the user is done.
3. Production and writes as canyon db query says: production only when the user asks for it by name, with their own reason, and --write only on a statement the user confirmed.
`

// dbListener is one run's listener: the database session it relays to, the token clients sign in with, and what
// the run has carried.
type dbListener struct {
	a       *app
	client  *Client
	session dbSession
	app     string
	token   string // the sign-in token when there is no file (path "")
	path    string // the file the token is read from at each sign-in (signInToken)
	cert    tlsCert
	ended   chan string   // the close_reason of the end that stops the run, once
	poll    time.Duration // how often watch reads the database session
	reader  chan struct{} // tests only: a relay's reader waits for it to close before it starts; nil starts at once

	mu       sync.Mutex        // one line at a time, from any connection
	live     sync.WaitGroup    // serve and each connection it carries, which stop waits for
	connsMu  sync.Mutex        // guards conns and stopping
	conns    map[net.Conn]bool // the clients being carried, which stop closes
	stopping bool              // stop has begun: no client is carried any more
	count    atomic.Int64
	fromDB   atomic.Int64
	verified sync.Once // a client refusing the certificate is said once a run
	keys     sync.Map  // each live connection's backend pid to the cancel key the listener gave its client
}

// say prints one line of the run's: to stdout on a terminal, to stderr under ai and json, whose stdout is the
// answer alone; and to stderr, always, when it is a failure.
func (l *dbListener) say(failure bool, f string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if failure || l.a.render.Format == FormatAI || l.a.render.Format == FormatJSON {
		l.a.errf(f, v...)
		return
	}
	l.a.printf(f, v...)
}

// end stops the run on the database session's end, the first one said.
func (l *dbListener) end(reason string) {
	select {
	case l.ended <- reason:
	default:
	}
}

// endLine is the line a mid-session end prints (§9), by the contract's close_reason; one this build does not know
// still ends the run.
func (l *dbListener) endLine(reason string) string {
	s := l.session
	switch reason {
	case "signed_out":
		return "Database session ended: your Canyon sign-in expired. Run: canyon login " + l.client.name()
	case "ended":
		return "Database session ended by an owner or admin of " + cell(l.app) + "."
	case "idle":
		return fmt.Sprintf("Database session ended after %d min idle.", s.IdleTimeout/60)
	case "closed":
		return "Database session ended: you ended it."
	case "expired":
		hours := 0.0
		opened, err1 := time.Parse(time.RFC3339, s.OpenedAt)
		expires, err2 := time.Parse(time.RFC3339, s.ExpiresAt)
		if err1 == nil && err2 == nil {
			hours = expires.Sub(opened).Hours()
		}
		return fmt.Sprintf("Database session ended: it reached its time limit, %s h.", strconv.FormatFloat(hours, 'f', -1, 64))
	case "access_changed":
		return "Database session ended: your access to " + cell(l.app) + " changed."
	case "setting_changed":
		return "Database session ended: an admin changed Settings › Production data."
	case "database_gone":
		return "Database session ended: the app's database isn't available any more."
	case "held":
		return "Database session ended: production was put on hold."
	case "turned_off":
		return "Database session ended: an admin turned database sessions off."
	case "replaced":
		return "Database session ended: a " + dbModeWord(map[string]string{"read": "write", "write": "read"}[s.Mode]) + " session replaced it."
	}
	return "Database session ended for a reason this canyon doesn't know: update the CLI."
}
