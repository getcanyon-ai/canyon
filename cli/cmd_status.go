package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// statusRead is GET /api/v1/projects/:id/status (contracts/routes.md), decoded into fields and never walked as a map.
type statusRead struct {
	Envs       map[string]*env `json:"envs"`
	LastRun    *string         `json:"last_run"`    // the app's own newest run, null when none: never a check's or a pull request's
	PromoteRun *string         `json:"promote_run"` // the run the promote button would send, null when none has looked
	Receipt    receipt         `json:"receipt"`
	Access     *teamAccess     `json:"-"` // the project read's, GET /api/v1/projects/:id: this read carries none
	Admin      bool            `json:"-"` // the fleet read's viewer, asked below contributor or when the host has a warning; false where sign-in is off
	Approved   bool            `json:"-"` // the project read's stage is approved, on the newest run's package
	SignInOff  bool            `json:"-"` // the host's /auth/config: nobody signs in, so anyone who reaches it opens the app
	// HostWarning is the front door's warning (GET /api/v1/front-door): apps on this cluster reach the
	// server they run on, which never stopped the door being saved (#1435). "" without one. Shown to an
	// admin alone, or where nobody signs in: only an admin can change the cluster's firewall.
	HostWarning string `json:"-"`
	// DiscoveryNote is the front door's discovery_note (GET /api/v1/front-door): Canyon has not read a choice 2
	// provider's discovery yet, so sign-in under allowlist is refused (#2344), or its daily read of it failed (#2389).
	// "" without one. Shown as HostWarning is.
	DiscoveryNote string `json:"-"`
	// Kube is the front door's runtime, kubernetes (GET /api/v1/front-door): an app there has no network to
	// recreate, and an env is floored at its next deploy or promotion (design/EGRESS.md §8.1).
	Kube bool `json:"-"`
	// Earlier is production's earlier versions, newest first (contracts/routes.md); nil from a host before it.
	Earlier []earlier `json:"earlier"`
	// Scanners is how old the host's vulnerability databases are (GET /api/v1/scanners/data, an admin's read;
	// #2743): nil for anyone else, an older host and a read that fails, none of which status names.
	Scanners []scanDB `json:"-"`
	// Outbound is what the outbound row reads (design/EGRESS.md §8.1, §8.2); nil below contributor but an admin,
	// and under json, which carries no row.
	Outbound *outbound `json:"-"`
	// Provided is what the org's platform provisioned for the app, the project read's resources[] in
	// state provided, in the host's order (contracts/routes.md, #2432); the reduced row carries none.
	Provided []resource `json:"-"`
	// Platform is the platform row (design/PATCHING.md §8): the most pressing environment's patch state in words,
	// nil where neither carries one, below contributor among them; json carries the states themselves.
	Platform *platformLine `json:"-"`
}

// resource is one row of the project read's resources[] (contracts/routes.md): endpoint on provided alone.
type resource struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Env      string `json:"env"`
	State    string `json:"state"`
	Endpoint string `json:"endpoint"`
}

// provided is the rows the org's platform provided, in the host's order.
func provided(rows []resource) []resource {
	return slices.DeleteFunc(rows, func(r resource) bool { return r.State != "provided" })
}

// outbound is what the app called out to (GET /api/v1/projects/:id/egress, contracts/routes.md, Egress
// control) and the host's via (GET /api/v1/egress), which every member reads.
type outbound struct {
	Unread string                  `json:"-"` // why either read failed: the row says so, and nothing else
	Via    string                  `json:"-"`
	Floor  *string                 `json:"floor"` // enforced, not_enforced, or null: the app's network
	Envs   map[string]*outboundEnv `json:"envs"`
}

type outboundEnv struct {
	Via          *string `json:"via"`  // the egress spec its running version was deployed under, null where nothing is
	Mode         string  `json:"mode"` // observe or allowlist: its own, else the org's
	Destinations []struct {
		Destination string `json:"destination"`
		Port        int    `json:"port"`
		LastSeenAt  string `json:"last_seen_at"`
		// the last call Canyon refused while adding the destination's credential, nil for none, and from a host
		// before #2692, which recorded none (contracts/routes.md, the app's egress read)
		LastCredentialRefusal *struct {
			Reason string `json:"reason"`
			At     string `json:"at"`
		} `json:"last_credential_refusal"`
	} `json:"destinations"`
	Requests []outboundRequest `json:"requests"` // pending first, then dismissed, each the latest refused first
	Allows   []outboundAllow   `json:"allows"`   // who allowed each entry and when, by entry; none from a host before #2722
}

// outboundAllow is who allowed one entry of an env's allowlist and when, and the latest refusal of the requests it
// closed, nil when it closed none (contracts/routes.md, the egress read's allows[]; #2722).
type outboundAllow struct {
	Entry         string   `json:"entry"`
	By            actorRef `json:"by"`
	At            string   `json:"at"`
	LastRefusedAt *string  `json:"last_refused_at"`
}

// allowedNow is the env's Allows that say its calls go through now (cli.md §6.1.5): each since its version went live at
// deployed that closed a refusal, while the env is under allowlist and running. Only a refusal proves the app's client
// goes through the gateway, under observe nothing was refused, and a held or stopped env makes no call (#2722). A time
// that does not read is never read as older, as refusedSince reads one.
func (e *outboundEnv) allowedNow(deployed, state string) []outboundAllow {
	if e.Mode != "allowlist" || state != "running" {
		return nil
	}
	since, err := time.Parse(time.RFC3339, deployed)
	return slices.DeleteFunc(slices.Clone(e.Allows), func(a outboundAllow) bool {
		at, bad := time.Parse(time.RFC3339, a.At)
		return a.LastRefusedAt == nil || err == nil && bad == nil && at.Before(since)
	})
}

// name is who an actor is in words, everywhere the CLI prints one: its display name, else, for the one caller of a
// host with sign-in off, D5's someone not signed in, as the dashboard says it (web/src/ui/Actor.tsx), else its
// subject (#2705).
func (r actorRef) name() string {
	switch {
	case r.Display != "":
		return r.Display
	case r.Kind == "person" && r.Subject == "anonymous":
		return "someone not signed in"
	}
	return r.Subject
}

// allowedBy is an Allow as an agent reads it, on canyon status and in a log alike: the entry, who and when.
func allowedBy(a outboundAllow, dot string) string {
	return cell(a.Entry) + " " + dot + " by " + cell(a.By.name()) + " at " + cell(a.At)
}

// outboundRequest is one egress request of an env (design/EGRESS.md §3.4): a refusal count is its, as a row keeps
// only the last result.
type outboundRequest struct {
	Destination   string `json:"destination"`
	State         string `json:"state"`
	LastRefusedAt string `json:"last_refused_at"`
	Refusals      int64  `json:"refusals"`
}

// refusedSince is the env's requests last refused since its version went live at deployed. A time that does not
// read is never read as older: the request stays.
func (e *outboundEnv) refusedSince(deployed string) []outboundRequest {
	since, err := time.Parse(time.RFC3339, deployed)
	if err != nil {
		return e.Requests
	}
	return slices.DeleteFunc(slices.Clone(e.Requests), func(r outboundRequest) bool {
		at, err := time.Parse(time.RFC3339, r.LastRefusedAt)
		return err == nil && at.Before(since)
	})
}

// credentialRefusal is one destination's last call Canyon refused while adding its credential (#2692).
type credentialRefusal struct{ destination, reason, at string }

// credentialRefusedSince is each destination whose last credential refusal came since its version went live at
// deployed, in the read's order. A time that does not read is never read as older: the refusal stays.
func (e *outboundEnv) credentialRefusedSince(deployed string) []credentialRefusal {
	since, err := time.Parse(time.RFC3339, deployed)
	var out []credentialRefusal
	for _, d := range e.Destinations {
		r := d.LastCredentialRefusal
		if r == nil {
			continue
		}
		if at, bad := time.Parse(time.RFC3339, r.At); err == nil && bad == nil && at.Before(since) {
			continue
		}
		out = append(out, credentialRefusal{fmt.Sprintf("%s:%d", d.Destination, d.Port), r.Reason, r.At})
	}
	return out
}

// dismissed is whether a person dismissed the request.
func (r outboundRequest) dismissed() bool { return r.State == "dismissed" }

// teamAccess is `access` on the project read from contributor up (contracts/routes.md): the reader's
// level on the app and how far it is shared.
type teamAccess struct {
	Level    string `json:"level"`
	Everyone bool   `json:"everyone"`
	People   int    `json:"people"`
	Groups   int    `json:"groups"`
}

// counts are its people and groups said out loud, a zero part left out as the deploy line leaves it
// (contracts/events.md); "" when it is shared with no person and no group.
func (t teamAccess) counts(and string) string {
	var parts []string
	if t.People > 0 {
		parts = append(parts, plural(t.People, "person", "people"))
	}
	if t.Groups > 0 {
		parts = append(parts, plural(t.Groups, "group", "groups"))
	}
	return strings.Join(parts, and)
}

type env struct {
	URL        string    `json:"url,omitempty"`
	Package    string    `json:"package,omitempty"`
	Label      string    `json:"label,omitempty"`
	DeployedAt string    `json:"deployed_at,omitempty"`
	Pending    *pending  `json:"pending,omitempty"`
	Answered   *answered `json:"answered,omitempty"` // what the app answered Canyon's GET / after its deploy (contracts/routes.md)
	Missing    []string  `json:"missing,omitempty"`  // production's runtime values set for preview alone, on the status read (contracts/routes.md; #2786)
	State      string    `json:"state,omitempty"`    // whether it runs now: running, restarting, stopped or unknown; absent where the org runs it (contracts/routes.md)
	RunBy      string    `json:"run_by,omitempty"`   // canyon, or org where the org's platform runs it (design/HAND-OFF.md)
	SeenAt     string    `json:"seen_at,omitempty"`  // when a pod the org runs last checked in, absent before one has (contracts/routes.md, #2452)
	Restarting *struct {
		Restarts int `json:"restarts"`
	} `json:"restarting,omitempty"` // how often it was started again since deployed_at, on the status read alone
	Patch *envPatch `json:"patch,omitempty"` // Canyon's patch of it, from contributor up (contracts/routes.md, envs.<env>.patch)
}

// pending is the release an environment waits on (contracts/routes.md, envs.<env>.pending).
type pending struct {
	Event       string   `json:"event"`
	SentAt      string   `json:"sent_at"`
	State       string   `json:"state"`
	Via         string   `json:"via"`               // webhook | repo: how the release reaches the platform (#2470)
	Missing     []string `json:"missing,omitempty"` // what the platform's last deployed report on it left out (#2452)
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request,omitempty"` // the deploy repo's pull request it waits on (#2470)
}

// restarts is how often a restarting environment was started again since its deploy, as status says it:
// ` (7 times since 5 Oct 09:12)`, `once` for one; "" when the read carries no count.
func (e *env) restarts() string {
	if e.Restarting == nil {
		return ""
	}
	times := fmt.Sprintf("%d times", e.Restarting.Restarts)
	if e.Restarting.Restarts == 1 {
		times = "once"
	}
	since := cell(e.DeployedAt)
	if t, err := time.Parse(time.RFC3339, e.DeployedAt); err == nil {
		since = t.Local().Format("2 Jan 15:04")
	}
	return " (" + times + " since " + since + ")"
}

// missing is the names of the runtime values set for preview that production has none of, each as a cell, none for no
// environment (contracts/routes.md, envs.prod.missing; #2786).
func (e *env) missing() []string {
	if e == nil {
		return nil
	}
	names := make([]string, len(e.Missing))
	for i, name := range e.Missing {
		names[i] = cell(name)
	}
	return names
}

// answered is the words of what it answered beside readiness, "" for nothing to say or no environment.
func (e *env) answered() string {
	if e == nil {
		return ""
	}
	return e.Answered.beside()
}

// flagged is answered's for a step an agent follows: Canyon's words alone, the app's left out (flaggedWords).
func (e *env) flagged() string {
	if e == nil {
		return ""
	}
	return e.Answered.flaggedWords()
}

// running is the environment when something runs there, nil when nothing does: an environment waiting
// on its first release (release.schema.json's env_pending alone) has no url and runs nothing yet.
func (e *env) running() *env {
	if e == nil || e.URL == "" {
		return nil
	}
	return e
}

// line is how the release an environment waits on is said (contracts/routes.md, envs.<env>.pending): in the
// deploy repo's words when it is a commit (design/EXPERIENCE.md, the environment's pending line under delivery: repo).
func (p *pending) line() string {
	if p.Via == "repo" {
		return p.repoLine()
	}
	switch {
	case p.State == "undelivered":
		return "Your platform did not accept the release"
	case p.State == "refused":
		return "Your platform refused the release"
	case p.Event == "remove":
		return "removing · waiting on your platform"
	case len(p.Missing) > 0: // its report left resources out: what the platform has to send (#2452)
		verb := " is"
		if len(p.Missing) > 1 {
			verb = " are"
		}
		return "Your platform's report left out where " + cell(strings.Join(p.Missing, ", ")) + verb
	}
	return "waiting on your platform since " + p.since()
}

// since is when the release was sent, as a person's clock says it.
func (p *pending) since() string {
	if t, err := time.Parse(time.RFC3339, p.SentAt); err == nil {
		return cell(t.Local().Format("15:04"))
	}
	return cell(p.SentAt)
}

// repoLine is a delivery: repo release's pending line: committed, waiting on a pod or on a merge.
func (p *pending) repoLine() string {
	switch {
	case p.State == "undelivered":
		return "Canyon couldn't commit the release"
	case p.State == "refused":
		return "Your deploy repo refused the release"
	case p.Event == "remove" && p.PullRequest != nil:
		return "removing · waiting on your merge"
	case p.Event == "remove":
		return "removing · waiting on your platform"
	case p.PullRequest != nil:
		return "waiting on your merge since " + p.since()
	}
	return "waiting for a pod to check in since " + p.since()
}

// pulled is the pull request line under a pending line, "" when it waits on none.
func (p *pending) pulled() string {
	if p == nil || p.PullRequest == nil {
		return ""
	}
	return "pull request: " + cell(p.PullRequest.URL)
}

// runRow is one row of GET /api/v1/projects/:id/gate-runs. The single run read carries no
// timestamp, so the gate row is this one, and it prints its own id: the receipt can name another.
type runRow struct {
	ID        string `json:"id"`
	Package   string `json:"package"`
	State     string `json:"state"`
	Blocking  int    `json:"blocking"`
	Warnings  int    `json:"warnings"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
}

func runStatus(ctx context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("status takes no arguments, and got %q", rest[0]))
	}
	p, err := a.project() // status reads the file; writing it is deploy's alone (§3.2)
	if err != nil {
		return a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	at := "/api/v1/projects/" + url.PathEscape(p.App)
	var project struct {
		Project struct {
			Owners    []actorRef                       `json:"owners"`
			Summary   string                           `json:"summary"`
			Access    *teamAccess                      `json:"access"`
			Latest    *struct{ Package, Stage string } `json:"latest"`    // a reduced row never carries it (contracts/routes.md, App teams)
			Resources []resource                       `json:"resources"` // nor these: technical detail (design/PERMISSIONS.md)
		} `json:"project"`
	}
	if err := lacking(client.get(ctx, at, &project), p.App, client); err != nil { // guard (a), from the app's own read (§3.2)
		return a.fail(err)
	}
	if pr := project.Project; pr.Latest == nil && (pr.Access == nil || pr.Access.Level != "none") {
		if a.render.Format == FormatJSON {
			return a.json(statusJSON{App: p.App, Owners: pr.Owners, Summary: &pr.Summary})
		}
		a.renderReduced(p.App, pr.Owners, pr.Summary) // read-only: what its app page says; no access: the status read's refusal below
		return exitOK
	}
	var read statusRead
	if err := client.get(ctx, at+"/status", &read); err != nil {
		return a.fail(lacking(err, p.App, client)) // its 404 is an app the host no longer has: guard (a) (§3.2)
	}
	latest, err := ownRun(ctx, client, at, read.LastRun)
	if err != nil {
		return a.fail(lacking(err, p.App, client)) // as the status read's
	}
	read.Access, read.Provided = project.Project.Access, provided(project.Project.Resources)
	if read.SignInOff, err = a.signInOff(ctx, client); err != nil { // where nobody signs in, nobody's place on a team is the answer (D5)
		return a.fail(err)
	}
	var door struct {
		Runtime       string `json:"runtime"`
		Warning       string `json:"warning"`
		DiscoveryNote string `json:"discovery_note"` // absent from a host before #2344: none
	}
	if err := client.get(ctx, "/api/v1/front-door", &door); err != nil {
		return a.fail(err)
	}
	read.HostWarning, read.DiscoveryNote, read.Kube = door.Warning, door.DiscoveryNote, door.Runtime == "kubernetes"
	// D1: the newest run is the approved package's, so it reads approved, never blocked
	read.Approved = latest != nil && latest.State == "blocked" && project.Project.Latest.Stage == "approved" && project.Project.Latest.Package == latest.Package
	// Below contributor only an admin reads a status, and the host's warnings are an admin's to act on (cli.md §6.1):
	// the scanners' data among them, whose read refuses anyone else and audits each refusal, so only an admin asks it.
	below := read.Access != nil && read.Access.Level != "owner" && read.Access.Level != "contributor"
	if !read.SignInOff && (below || a.render.Format != FormatJSON) { // json carries no warning
		var fleet struct {
			Viewer struct {
				Role string `json:"role"`
			} `json:"viewer"`
		}
		// Below contributor, or with a host warning to place, the role decides what status shows, so a read that fails
		// fails it, as before. Otherwise only the scanners' warning hangs on it: a read that fails is no admin, and the
		// rest of status stands (#2743).
		needed := below || read.HostWarning != "" || read.DiscoveryNote != ""
		if err := client.get(ctx, "/api/v1/fleet?limit=1", &fleet); err != nil && needed {
			return a.fail(err)
		}
		read.Admin = fleet.Viewer.Role == "admin"
	}
	// The row is a person's and an agent's (json carries none), and a contributor's and up or an admin's: a
	// responder with no grant is never sent to the app's records (design/PERMISSIONS.md, Outbound).
	if a.render.Format != FormatJSON && (!below || read.Admin || read.SignInOff) {
		read.Outbound = outboundOf(ctx, client, at)
	}
	if a.render.Format == FormatJSON {
		return a.json(statusJSON{App: p.App, Envs: read.Envs, LastRun: latest, Receipt: &read.Receipt, Earlier: read.Earlier})
	}
	read.Platform = platformOf(ctx, client, a.clock, at, read, a.clock.now(), a.ago)
	if read.Admin || read.SignInOff { // where nobody signs in, the one caller is the anonymous admin
		read.Scanners = scannerData(ctx, client)
	}
	a.renderStatus(p.App, client.page(), read, latest, a.clock.now())
	return exitOK // a read carries no verdict: check and deploy do
}

// scanDB is one vulnerability database's age, as the host's scanner data read answers it (#2743).
type scanDB struct {
	Tool    string `json:"tool"`
	BuiltAt string `json:"built_at"`
	StaleAt string `json:"stale_at"`
}

// scannerData is how old the host's vulnerability databases are: an admin's read, so nil for anyone else, and for
// an older host or a read that fails, which status never names.
func scannerData(ctx context.Context, c *Client) []scanDB {
	var read struct {
		Databases []scanDB `json:"databases"`
	}
	if c.get(ctx, "/api/v1/scanners/data", &read) != nil {
		return nil
	}
	return read.Databases
}

// scanData is each database by the name a person knows it by, and the tool a review names when it can't check.
var scanData = map[string][2]string{
	"trivy":      {"trivy's vulnerability data", "trivy"},
	"trivy-java": {"trivy's Java data", "trivy"},
	"grype":      {"grype's vulnerability data", "grype"},
}

// staleSoon is how long before a database turns stale status warns of it: a day, to fix what stops its refresh.
const staleSoon = 24 * time.Hour

// words is what status says of a database that turns stale within staleSoon of now, or is stale: the time it does
// for an agent, how long until then for a person; "" for any other, one with no age and a tool status doesn't know.
func (d scanDB) words(now time.Time, ai bool) string {
	names, known := scanData[d.Tool]
	stale, err := time.Parse(time.RFC3339, d.StaleAt)
	switch {
	case !known || err != nil || stale.Sub(now) > staleSoon:
		return ""
	case !now.Before(stale) && ai:
		return names[0] + " is stale: reviews can't check with " + names[1] + " until Canyon refreshes it"
	case !now.Before(stale):
		return names[0] + " is over 5 days old, so reviews can't check with " + names[1] + " until Canyon refreshes it. Canyon's logs say why it couldn't."
	case ai:
		return names[0] + " goes stale at " + stale.UTC().Format(time.RFC3339) + ": reviews can't check with " + names[1] + " after that"
	}
	left := stale.Sub(now).Round(time.Minute)
	in := fmt.Sprintf("%dh", int(left.Hours()))
	if left < time.Hour {
		in = fmt.Sprintf("%dm", int(left.Minutes()))
	}
	return names[0] + " goes stale in " + in + ", and reviews can't check with " + names[1] + " after that. Canyon couldn't refresh it: its logs say why."
}

// outboundOf reads the outbound row's answers: the app's records and the host's via. A read that fails —
// an older host's 404, a 500, a network error — fails the row alone, which says so: status still renders
// everything else, and never draws a row it could not read.
func outboundOf(ctx context.Context, c *Client, at string) *outbound {
	var o outbound
	var host struct {
		Via string `json:"via"`
	}
	if err := c.get(ctx, at+"/egress", &o); err != nil {
		return &outbound{Unread: err.Error()}
	}
	if err := c.get(ctx, "/api/v1/egress", &host); err != nil {
		return &outbound{Unread: err.Error()}
	}
	o.Via = host.Via
	return &o
}

// statusJSON is status under --format json (design/specs/cli.md §5.4): what runs where, the app's own
// newest run and the promote receipt, as the host answered them; a read-only reader's is the app's
// owners and summary alone, as its human rendering is.
type statusJSON struct {
	App     string          `json:"app"`
	Envs    map[string]*env `json:"envs,omitempty"`
	LastRun *runRow         `json:"last_run"`
	Receipt *receipt        `json:"receipt,omitempty"`
	Earlier []earlier       `json:"earlier,omitempty"` // as the host answered it
	Owners  []actorRef      `json:"owners,omitempty"`
	Summary *string         `json:"summary,omitempty"`
}

// json writes v as one object and a newline on stdout, whole, and nothing else (cli.md §5.4).
func (a *app) json(v any) int {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return a.usage(err)
	}
	_, _ = a.out.Write(body.Bytes())
	return exitOK
}

// ownRun is the runs list's row for the app's own newest run, which the status read names: the list
// also holds the runs of checks and pull requests, which are never the app's review (contracts/routes.md),
// so the row is found by its id, a page at a time. nil when the app has none.
func ownRun(ctx context.Context, c *Client, at string, id *string) (*runRow, error) {
	if id == nil {
		return nil, nil
	}
	cursor := ""
	for {
		var page struct {
			Runs []runRow `json:"runs"`
			Next *string  `json:"next"`
		}
		if err := c.get(ctx, at+"/gate-runs?limit=200"+cursor, &page); err != nil {
			return nil, err
		}
		for i := range page.Runs {
			if page.Runs[i].ID == *id {
				return &page.Runs[i], nil
			}
		}
		if page.Next == nil {
			return nil, fmt.Errorf("%s names %s as the app's newest run and does not list it", c.Base, visible(*id))
		}
		cursor = "&cursor=" + url.QueryEscape(*page.Next)
	}
}

const statusLabel = 10 // the row label's column: "production", the longest of the four

func (a *app) renderStatus(name, host string, s statusRead, latest *runRow, now time.Time) {
	if a.render.Format == FormatAI {
		a.aiStatus(name, host, s, latest, now)
		return
	}
	a.printf("\n  %s\n\n", a.render.paint(ansiBold, cell(name)+" "+a.dot()+" "+cell(s.Receipt.Org)))
	preview, prod := s.Envs["preview"], s.Envs["prod"]
	width := max(utf8.RuneCountInString(a.version(preview)), utf8.RuneCountInString(a.version(prod)))
	a.envRow("preview", "preview", preview, width, "nothing running", now)
	a.envRow("production", "prod", prod, width, "not promoted yet", now) // the dashboard's word; --env prod stays the flag
	for _, name := range prod.missing() {                                // beside what production answered: a value it lacks may be why it answers 500 (#2786)
		a.row("", a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" "+name+" is set for preview but not production. Set it with: canyon secret set "+name+" --env prod")) // under production's cells, as its own
	}
	a.platformRow(s.Platform)
	a.providedRows(s.Provided)
	a.accessRow(s)
	a.reviewRow(latest, s.Approved, now)
	a.outboundRow(s, now)
	a.checklist(s.Receipt.withPatch(s.Envs["preview"]))
	if s.HostWarning != "" && (s.Admin || s.SignInOff) { // where nobody signs in, whoever reads is the one who runs it
		a.printf("\n  %s\n", a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" "+cell(s.HostWarning)+" The rule is in deploy/k8s/README.md, under Host firewall."))
	}
	if s.DiscoveryNote != "" && (s.Admin || s.SignInOff) {
		a.printf("\n  %s\n", a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" "+cell(s.DiscoveryNote)))
	}
	for _, d := range s.Scanners {
		if said := d.words(now, false); said != "" {
			a.printf("\n  %s\n", a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" "+said))
		}
	}
	a.earlierLines(s.Earlier, s.Envs["prod"].running(), s.Receipt.Held != nil)
	a.history(host, name)
}

// renderReduced is the app as a read-only member reads it (design/PERMISSIONS.md): its name, owners and
// summary, then EXPERIENCE D5's line, and nothing of its runs, findings or environments.
func (a *app) renderReduced(name string, owners []actorRef, summary string) {
	names := make([]string, len(owners))
	for i, o := range owners {
		names[i] = cell(o.Display)
	}
	if summary == "" {
		summary = "No description yet"
	}
	if a.render.Format == FormatAI {
		a.printf("CANYON STATUS\n\napp: %s\nowners: %s\nsummary: %s\naccess: read-only\n", cell(name), a.orElse(strings.Join(names, ", ")), cell(summary))
		return
	}
	a.printf("\n  %s\n\n", a.render.paint(ansiBold, cell(name)))
	a.row("owners", a.orElse(strings.Join(names, ", ")))
	a.row("summary", cell(summary))
	a.printf("\n  Your access: read-only.\n")
}

func (a *app) envRow(label, flag string, e *env, width int, absent string, now time.Time) {
	version := pad(a.version(e), width)
	if e != nil && e.running() == nil && e.Pending != nil { // waiting on its first release: what it waits on, never a URL
		a.row(label, version, e.Pending.line())
		if pr := e.Pending.pulled(); pr != "" {
			a.row("", pad("", width), a.faint(pr))
		}
		return
	}
	if e == nil {
		a.row(label, version, absent) // no URL is built for an environment the host does not name
		return
	}
	switch e.State { // what runs now, never what the last deploy recorded (#1241)
	case "restarting": // no URL: it may be down between two restarts (#1722)
		a.row(label, version, a.render.paint(ansiRed, a.render.glyph(stateFail)+" keeps restarting"+e.restarts()), a.faint("canyon logs --env "+flag+" says why"))
	case "stopped":
		a.row(label, version, a.render.paint(ansiRed, a.render.glyph(stateFail)+" stopped"), a.faint("canyon logs --env "+flag+" says why"))
	case "running", "": // "": the org runs it, and Canyon has no state to say
		a.row(label, version, a.render.paint(ansiCyan+ansiUnderline, cell(e.URL)), a.faint(a.ago(e.DeployedAt, now)), e.answered(), a.patchedBy(e, now))
	default: // unknown, or a state this build does not know: never drawn live
		a.row(label, version, a.render.paint(ansiCyan+ansiUnderline, cell(e.URL)), a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" couldn't check whether it runs"), e.answered())
	}
	if line, warn := a.checkIn(e, now); warn { // the org's pods, under what runs and above what it waits on (#2452)
		a.row("", pad("", width), a.render.paint(ansiAmber, a.render.glyph(stateWarn)+" "+line))
	} else if line != "" {
		a.row("", pad("", width), a.faint(line))
	}
	if e.Pending != nil { // a release open over what runs, said under it: never silently gone (HAND-OFF §4)
		a.row("", pad("", width), e.Pending.line())
		if pr := e.Pending.pulled(); pr != "" {
			a.row("", pad("", width), a.faint(pr))
		}
	}
}

// Every surface reads a check-in by one rule (contracts/routes.md, envs.<env>.seen_at; #2452): fresh while it
// is within checkInStale, and a check-in that never came is said once checkInNever has passed since the report.
const (
	checkInStale = 5 * time.Minute
	checkInNever = 10 * time.Minute
)

// checkIn is what an environment the org runs says of its pods, against now, and whether it is a warning: a
// pod checked in, none since a time, or none at all once a pod should be up; "" for one Canyon runs, and
// within the first minutes after the report, while a pod may still be starting. A check-in ahead of now is
// now. It is the pods' sign-in sidecar, never the app: it is never said as running.
func (a *app) checkIn(e *env, now time.Time) (string, bool) {
	if e == nil || e.RunBy != "org" || e.URL == "" {
		return "", false
	}
	if seen, err := time.Parse(time.RFC3339, e.SeenAt); err == nil {
		if !e.silent(now) {
			if e.Pending != nil && e.Pending.Event == "remove" { // its pods pull a list that admits nobody: one still pulling is one the platform has not taken down (#2450)
				return "a pod still checked in " + a.ago(e.SeenAt, now), true
			}
			return "a pod checked in " + a.ago(e.SeenAt, now), false
		}
		at := seen.Local()
		since := at.Format("15:04")
		if y, m, d := at.Date(); y != now.Local().Year() || m != now.Local().Month() || d != now.Local().Day() { // past midnight, the day too
			since = at.Format("2 Jan 15:04")
		}
		return "no pod has checked in since " + since, true
	}
	if e.silent(now) {
		return "reported deployed, but no pod has checked in", true
	}
	return "", false
}

// silent is whether the org's pods of e have stopped checking in, or never began, against now: its last
// check-in older than checkInStale, or none once checkInNever has passed since the report. Whoever runs it:
// the callers ask it of an environment the org runs.
func (e *env) silent(now time.Time) bool {
	if seen, err := time.Parse(time.RFC3339, e.SeenAt); err == nil {
		return now.Sub(seen) > checkInStale
	}
	deployed, err := time.Parse(time.RFC3339, e.DeployedAt)
	return err == nil && now.Sub(deployed) > checkInNever
}

// providedRows is §6.1's resources rows (#2432): each database or bucket the org's platform provisioned,
// with where the platform says it is, as text and never painted as a link: it is no address a person opens.
func (a *app) providedRows(rows []resource) {
	for i, r := range rows {
		label := ""
		if i == 0 {
			label = "resources"
		}
		a.row(label, a.providedLine(r), a.faint("your platform's"))
	}
}

// providedLine is one provided resource in words: its type and name, its environment, its endpoint.
func (a *app) providedLine(r resource) string {
	env := map[string]string{"prod": "production"}[r.Env]
	if env == "" {
		env = cell(r.Env)
	}
	return cell(r.Type+" "+r.Name) + " " + a.dot() + " " + env + " " + a.dot() + " " + cell(r.Endpoint)
}

// accessRow is §6.1's line: how far the app is shared, then the reader's place on its team.
func (a *app) accessRow(s statusRead) {
	if line := a.accessLine(s); line != "" {
		a.row("access", line)
	}
}

// accessSignInOff is who can open an app on a host where nobody signs in (cli.md §5.3, §6.1).
const accessSignInOff = "anyone who can reach this Canyon (sign-in is off)"

// accessLine is that line's words, "" when the project read answered no access.
func (a *app) accessLine(s statusRead) string {
	t := s.Access
	if t == nil { // a project read that answers no access has no line to give
		return ""
	}
	if s.SignInOff { // private, shared and "you are an admin" all mean nothing where nobody signs in
		return accessSignInOff
	}
	line := "private"
	switch counts := t.counts(" and "); {
	case t.Everyone:
		line = "shared with everyone at " + cell(s.Receipt.Org)
	case counts != "":
		line = "shared with " + counts
	}
	you := map[string]string{"owner": "an owner", "contributor": "a contributor"}[t.Level]
	if you == "" && s.Admin {
		you = "an admin"
	}
	if you != "" {
		line += " " + a.dot() + " you are " + you
	}
	return line
}

func (a *app) reviewRow(latest *runRow, approved bool, now time.Time) {
	if latest == nil {
		state, colour := a.gateCell("never_reviewed")
		a.row("review", a.render.paint(colour, state))
		return
	}
	when := latest.EndedAt // when the run ended, else when its row was written
	if when == "" {
		when = latest.StartedAt
	}
	gate := latest.State
	if approved {
		gate = "approved"
	}
	state, colour := a.gateCell(gate)
	counts := plural(latest.Blocking, "blocking finding", "blocking findings") + ", " + plural(latest.Warnings, "warning", "warnings")
	a.row("review", a.render.paint(colour, state), counts, a.faint(a.ago(when, now))) // no run id: a person reads the version and the age (cli.md §6.1)
}

// outboundEnvs are the envs an outbound row says, in its order.
var outboundEnvs = []struct{ name, key string }{{"preview", "preview"}, {"production", "prod"}}

// outboundHidden is whether status says nothing of outbound: below contributor, or while nothing of the app goes
// through Canyon, the host's via off and its network open.
func (o *outbound) outboundHidden() bool {
	return o == nil || o.Unread == "" && o.Via != "canyon" && o.Floor == nil
}

// outboundSaid is what an env reads short of its records, §8.1's value; "" for one through Canyon, whose records say
// the rest. The org's platform runs its own egress; a network not read as enforced is an admin's to recreate before a
// deploy can route through Canyon (fail closed: unknown, or none under via canyon, never reads recorded); and a
// version deployed before via canyon reaches it by a deploy.
func outboundSaid(s statusRead, ran *env, rec *outboundEnv) string {
	o := s.Outbound
	through := rec != nil && rec.Via != nil && *rec.Via == "canyon"
	perEnv := s.Kube && o.Floor != nil && *o.Floor == "not_enforced" // on Kubernetes the floor is each env's own pod's
	switch {
	case ran.RunBy == "org":
		return "runs on your own platform"
	case perEnv && !through, s.Kube && !perEnv && (o.Floor == nil || *o.Floor != "enforced"): // no network to recreate; a floor this build does not know, or none, is never recorded
		return "may skip Canyon until its next deploy or promotion"
	case !s.Kube && (o.Floor == nil || *o.Floor != "enforced"):
		return "may skip Canyon until an admin recreates its network in Settings › Outbound"
	case through:
		return ""
	}
	return "not yet through Canyon: deploy again"
}

// outboundRow is what each env the status read names calls out to, in §8.1's values (design/EGRESS.md). One through
// Canyon says its mode, then what it refused since its version went live, a line each, else how many destinations it
// reached in the week. Outbound is beta: the row's first line ends (beta).
func (a *app) outboundRow(s statusRead, now time.Time) {
	o := s.Outbound
	if o != nil && o.Unread != "" {
		a.row("outbound", a.render.paint(ansiAmber, "couldn't read: "+cell(o.Unread))+" "+betaMark)
		return
	}
	if o.outboundHidden() {
		return
	}
	label, mark := "outbound", " "+betaMark
	for _, e := range outboundEnvs {
		ran, rec := s.Envs[e.key], o.Envs[e.key]
		if ran == nil {
			continue
		}
		said := outboundSaid(s, ran, rec)
		var refused []outboundRequest
		var allowed []outboundAllow
		if said == "" {
			refused, allowed = rec.refusedSince(ran.DeployedAt), rec.allowedNow(ran.DeployedAt, ran.State)
			said = a.outboundRecords(rec, len(refused), a.version(ran), now)
		}
		a.row(label, e.name+": "+said+mark)
		label, mark = "", ""
		a.refusedRows(refused, allowed, now)
	}
}

// modeWords is an env's mode as both formats say it (#2890): only allowed destinations under allowlist, else recorded.
func modeWords(mode string) string {
	if mode == "allowlist" {
		return "only allowed destinations"
	}
	return "recorded"
}

// outboundRecords is §8.1's value of an env through Canyon, refused the destinations it refused since version went live.
func (a *app) outboundRecords(rec *outboundEnv, refused int, version string, now time.Time) string {
	mode := modeWords(rec.Mode)
	if refused > 0 {
		return fmt.Sprintf("%s %s %s refused since %s went live", mode, a.dot(), plural(refused, "destination", "destinations"), version)
	}
	seen := 0
	for _, d := range rec.Destinations {
		if t, err := time.Parse(time.RFC3339, d.LastSeenAt); err == nil && !t.Before(now.Add(-outboundWeek)) {
			seen++
		}
	}
	return mode + " " + a.dot() + " " + plural(seen, "destination", "destinations") + " in the last 7 days"
}

// refusedRows are §8.1's lines under an env, one a destination refused and then one each allowedNow names (#2722),
// the names in one column.
func (a *app) refusedRows(refused []outboundRequest, allowed []outboundAllow, now time.Time) {
	width := 0
	for _, r := range refused {
		width = max(width, utf8.RuneCountInString(cell(r.Destination)))
	}
	for _, al := range allowed {
		width = max(width, utf8.RuneCountInString(cell(al.Entry)))
	}
	for _, r := range refused {
		a.row("", "  "+a.render.paint(ansiRed, a.render.glyph(stateFail))+" "+pad(cell(r.Destination), width)+"   "+a.refusedWords(r, now))
	}
	for _, al := range allowed {
		when := a.ago(al.At, now)
		if when != "" {
			when = " " + when
		}
		a.row("", "  "+a.render.paint(ansiGreen, a.render.glyph(statePass))+" "+pad(cell(al.Entry), width)+"   allowed by "+cell(al.By.name())+
			when+" "+a.dot()+" calls go through now, no deploy needed")
	}
}

// refusedWords is how often a destination was refused and how long ago; a time that does not read says no age. The
// count is the refusals an Allow would lift, never every call, so it says not allowed, as the Outbound card does (#2718).
func (a *app) refusedWords(r outboundRequest, now time.Time) string {
	words := fmt.Sprintf("not allowed %d times", r.Refusals)
	if r.Refusals == 1 {
		words = "not allowed once"
	}
	switch ago := a.ago(r.LastRefusedAt, now); {
	case ago == "":
	case r.Refusals == 1, ago == "just now":
		words += ", " + ago
	default:
		words += ", last " + ago
	}
	if r.dismissed() {
		words += " (dismissed)"
	}
	return words
}

const outboundWeek = 7 * 24 * time.Hour

func (a *app) row(label string, cells ...string) {
	line := pad(label, statusLabel)
	for _, one := range cells {
		if one != "" {
			line += "   " + one
		}
	}
	a.printf("  %s\n", strings.TrimRight(line, " "))
}

// version names what an environment runs: its label, else its id without the store's `pkg_` prefix, which is no name a person says (contracts/routes.md).
func (a *app) version(e *env) string {
	switch {
	case e == nil, e.Label == "" && e.Package == "":
		return a.dash()
	case e.Label != "":
		return cell(e.Label)
	}
	return cell(strings.TrimPrefix(e.Package, "pkg_"))
}

// ago is the fleet's age, said out loud: a row on this screen has room for the word.
func (a *app) ago(ts string, now time.Time) string {
	switch age := a.age(ts, now); age {
	case a.dash():
		return "" // a timestamp that does not read is no age at all
	case "now":
		return "just now"
	default:
		return age + " ago"
	}
}

// aiStatus is §6.1's block under --format ai: the same reads as key: value lines, nothing painted and
// no age said, the receipt as §6.3's checklist, and an admin's host warning last.
func (a *app) aiStatus(name, host string, s statusRead, latest *runRow, now time.Time) {
	a.printf("CANYON STATUS\n\napp: %s\norg: %s\nhost: %s\n", cell(name), cell(s.Receipt.Org), cell(host))
	a.aiEnv("preview", "preview", s.Envs["preview"], "nothing running", now)
	a.aiEnv("production", "prod", s.Envs["prod"], "not promoted yet", now)
	if names := s.Envs["prod"].missing(); len(names) > 0 { // the promotion's line and step, said where the 500 is (#2786)
		a.printf("not set for production: %s\n  %s\n", strings.Join(names, ", "), a.missingStep())
	}
	if l := s.Platform; l != nil {
		a.printf("platform: %s\n", cmp.Or(l.Text, "couldn't read: "+cell(l.Unread)))
		if l.Approve != "" { // production's patch is asked: who decides it, and where (#2615)
			a.printf("  approval url: %s\n  %s\n", l.Approve, patchAsked)
		}
	}
	for _, r := range s.Provided {
		a.printf("resource: %s · your platform's\n", a.providedLine(r))
	}
	if line := a.accessLine(s); line != "" {
		a.printf("access: %s\n", line)
	}
	review := "never reviewed"
	if latest != nil {
		gate := latest.State
		if s.Approved {
			gate = "approved"
		}
		word, _ := a.gateCell(gate)
		_, word, _ = strings.Cut(word, " ") // the dashboard's word, without the glyph a person reads
		review = word + " · " + plural(latest.Blocking, "blocking finding", "blocking findings") + ", " + plural(latest.Warnings, "warning", "warnings") +
			" · run " + cell(latest.ID)
		if when := cmp.Or(latest.EndedAt, latest.StartedAt); when != "" {
			review += " (" + cell(when) + ")"
		}
	}
	a.printf("review: %s\nfleet url: %s\n\n", review, cell(host+"/#/apps/"+name))
	a.aiOutbound(name, host, s)
	a.aiChecklist(s.Receipt.withPatch(s.Envs["preview"]))
	if s.HostWarning != "" && (s.Admin || s.SignInOff) {
		a.printf("host warning: %s The rule is in deploy/k8s/README.md, under Host firewall.\n", cell(s.HostWarning))
	}
	if s.DiscoveryNote != "" && (s.Admin || s.SignInOff) {
		a.printf("discovery note: %s\n", cell(s.DiscoveryNote))
	}
	for _, d := range s.Scanners {
		if said := d.words(now, true); said != "" {
			a.printf("scanner data: %s\n", said)
		}
	}
	a.earlierLines(s.Earlier, s.Envs["prod"].running(), s.Receipt.Held != nil)
}

// What an agent reads of outbound (design/EGRESS.md §8.2): that a person allows a destination, in the dashboard, and
// that nothing it does reaches the destination another way, since that would go around the person who decides. An
// Allow reaches the running app with its next policy, every 5 s (§3.2), so a refused call needs no deploy. <url> is
// the app's card.
const (
	aiOutboundPerson = "Only a person on the app's team can allow a destination, in Canyon's dashboard. You cannot allow one.\n"
	aiOutboundNever  = `do not:
  - reach the destination another way: an IP address, another port, a DNS lookup, a raw socket
  - unset or change HTTPS_PROXY, HTTP_PROXY or NO_PROXY
  A person on the app's team decides what the app reaches; any other way goes around them.
`
	aiOutboundRefused = "outcome: Canyon refused these calls with HTTP 403. They never reached the destination.\n" + aiOutboundPerson + `do:
  1. Tell the user which destinations were refused and why the app needs each one.
  2. Give them this link: <url>
     Once a person allows a destination there, the app's next call to it goes through within seconds, with no deploy.
  3. If a destination is not needed, remove the call from the code and deploy again.
` + aiOutboundNever
	// aiOutboundTimeout fits a call that never reached the gateway, its client ignoring HTTPS_PROXY (§4.4): never a
	// refused one, which was a 403.
	aiOutboundTimeout = "If a call to an outside service times out, or can't resolve the host name, the client may be ignoring HTTPS_PROXY.\n"
	aiOutboundHeld    = aiOutboundPerson + `do:
  1. Tell the user what prod.egress-allowed says in the checklist, and why the app needs each destination it names.
  2. Give them this link: <url>
  3. If a destination isn't needed, or no one can allow it (an IP address, a one-word name, another port), change the code and deploy again. If it's still named, deploy once more after a few minutes.
` + aiOutboundNever
	// aiOutboundAllowed is what an agent does after an Allow (#2722), under one line an env naming what it allowed: in
	// run 3 of the #2162 journey it read the app's failures from before the Allow as current, then changed the app's HTTP
	// client and deployed again. Each line is its env's alone: another env may still refuse the same name.
	aiOutboundAllowedIn = "A person allowed these destinations in <env>: <names>. <Env>'s calls to them go through now, with no deploy.\n"
	aiOutboundAllowed   = `do:
  1. Before you change anything, check the app's next call to each one: wait a minute, then read the newest lines of canyon logs for that environment (--env preview or --env prod). What it logged before the Allow is old.
  2. Tell the user each destination is allowed there, and what the app's next call did.
do not:
  - change the code or deploy again for a destination a person allowed in that environment
`
	aiOutboundHeldOutcome = "outcome: prod.egress-allowed stops this version reaching production.\n"
	aiOutboundUnchecked   = "outcome: Canyon could not check outbound connections, so this version can't reach production. Do what the prod.egress-allowed line says. An approval won't clear this.\n"
)

// egressUnchecked is whether prod.egress-allowed holds r because it could not check: its line names no destination, so
// the steps for one say nothing true of it.
func (r receipt) egressUnchecked() bool {
	i := slices.IndexFunc(r.Rules, func(rule receiptRule) bool { return rule.ID == egressAllowed })
	return i >= 0 && slices.Contains(r.BlockedBy, egressAllowed) && strings.HasPrefix(r.Rules[i].Why, "could not check: ")
}

// outboundCard is the app's Outbound card, where a person allows a destination.
func outboundCard(name, host string) string { return cell(host + "/#/apps/" + name + "/outbound") }

// aiOutbound is §8.2's lines: one an env the status read names, then each destination it refused since its version
// went live; the block for an agent once any did, or what it needs while the go-live row blocks with nothing refused,
// as preview under observe and production under allowlist leave it; else, with an env through Canyon, the timeout
// hint. Nothing below contributor, or with nothing to say.
func (a *app) aiOutbound(name, host string, s statusRead) {
	o := s.Outbound
	if o == nil {
		return
	}
	refused, through, said := false, false, true
	var allowed []envAllowed
	switch {
	case o.Unread != "":
		a.printf("outbound: couldn't read: %s\n", cell(o.Unread))
	case o.outboundHidden():
		said = false
	default:
		refused, through, allowed = a.aiOutboundEnvs(s, outboundCard(name, host))
	}
	switch {
	case refused:
		a.printf("%s", strings.ReplaceAll(aiOutboundRefused, "<url>", outboundCard(name, host)))
	case s.Receipt.egressUnchecked():
		a.printf("%s", aiOutboundUnchecked)
	case slices.Contains(s.Receipt.BlockedBy, egressAllowed):
		a.printf("%s%s", aiOutboundHeldOutcome, strings.ReplaceAll(aiOutboundHeld, "<url>", outboundCard(name, host)))
	case through && len(allowed) == 0: // an Allow that closed a refusal proves the client goes through the gateway: the hint would send the agent after the wrong cause (#2722)
		a.printf("%s", aiOutboundTimeout)
	case !said:
		return
	}
	for _, e := range allowed {
		a.printf("%s", strings.NewReplacer("<env>", e.env, "<Env>", strings.ToUpper(e.env[:1])+e.env[1:], "<names>", strings.Join(e.names, ", ")).Replace(aiOutboundAllowedIn))
	}
	if len(allowed) > 0 {
		a.printf("%s", aiOutboundAllowed)
	}
	a.printf("\n")
}

// envAllowed is one env's destinations allowedNow names, as the block's line for that env names them.
type envAllowed struct {
	env   string
	names []string
}

// refusedCalls is a destination's refusals as an agent reads them, on canyon status and in a log alike: how many an
// Allow would lift, never every call (#2718), the last one's time, and whether a person dismissed it.
func refusedCalls(r outboundRequest, dot string) string {
	line := cell(r.Destination) + " " + dot + " not allowed " + plural(int(r.Refusals), "time", "times") + " " + dot + " last " + cell(r.LastRefusedAt)
	if r.dismissed() {
		line += " " + dot + " dismissed"
	}
	return line
}

// aiOutboundEnvs is one line an env, each destination it refused, each credential refusal since its version went live
// with what the agent does about it, and each allowedNow names; whether any was refused, whether any env goes through
// Canyon with none of its calls refused while adding a credential, and what each env allowed. url is the app's card.
func (a *app) aiOutboundEnvs(s statusRead, url string) (refusals, through bool, allows []envAllowed) {
	credentials := false
	o := s.Outbound
	for _, e := range outboundEnvs {
		ran, rec := s.Envs[e.key], o.Envs[e.key]
		if ran == nil {
			continue
		}
		if said := outboundSaid(s, ran, rec); said != "" {
			a.printf("outbound: %s · %s\n", e.name, said)
			continue
		}
		refused := rec.refusedSince(ran.DeployedAt)
		count := "no destination"
		if len(refused) > 0 {
			count = plural(len(refused), "destination", "destinations")
		}
		credential := rec.credentialRefusedSince(ran.DeployedAt)
		also := "" // never "no destination refused" alone while a credential refusal is named below it
		if len(credential) > 0 {
			also = " · refused while adding a credential to " + plural(len(credential), "destination", "destinations")
		}
		a.printf("outbound: %s · %s · %s refused since %s went live%s\n", e.name, modeWords(rec.Mode), count, a.version(ran), also)
		for _, r := range refused {
			a.printf("refused: %s\n", refusedCalls(r, "·"))
		}
		for _, r := range credential {
			a.printf("%s", aiCredentialRefused(r, url))
			credentials = true
		}
		var names []string
		for _, al := range rec.allowedNow(ran.DeployedAt, ran.State) {
			a.printf("allowed: %s · calls go through now, no deploy needed\n", allowedBy(al, "·"))
			names = append(names, cell(al.Entry))
		}
		if len(names) > 0 {
			allows = append(allows, envAllowed{e.name, names})
		}
		refusals, through = refusals || len(refused) > 0, true
	}
	return refusals, through && !credentials, allows // a call refused while adding a credential went through the gateway
}

// aiCredentialRefusals is, by reason, what the app was told and what an agent does about it (#2692; contracts/routes.md,
// the refusals' bodies): on a CONNECT most clients hide the body, so the agent sees only the status. The code is the
// agent's to fix; a credential, a person's to set.
var aiCredentialRefusals = map[string][2]string{
	"inject_requires_tls": {"Canyon refused this call: use https:// for <destination>. Canyon adds the app's credential only over https.",
		"Fix the code: call https://<destination>, not http://, then deploy again."},
	"misdirected": {"Canyon refused this call: the request was for <destination>, on a connection to another host.",
		"Fix the code: send requests for <destination> only on its own connection, and never set the Host header to another name. Then deploy again."},
	"inject_unavailable": {"Canyon could not add the app's credential for <destination>, so it did not send this call.",
		"Try the call again. If it keeps failing, ask a person on the app's team to set the credential for <destination> again: <url>. You cannot set it."},
	"upstream_failed": {"Canyon could not connect to <destination> or verify its certificate, so it did not send this call.",
		"Check <destination> is spelled right in the code. If it is, the destination is down or its certificate is not valid: tell the user. Canyon did not send the credential."},
}

// aiCredentialRefused is one destination's last credential refusal as an agent reads it: the reason and when, what
// the app was told, and what to do. A reason this build does not know says so and what it was, never guessing a fix.
func aiCredentialRefused(r credentialRefusal, url string) string {
	name, _, _ := strings.Cut(r.destination, ":")
	words, ok := aiCredentialRefusals[r.reason]
	if !ok {
		words = [2]string{"Canyon refused this call to <destination> while adding the app's credential.",
			"Tell the user, with this line. Don't change the code for it."}
	}
	say := strings.NewReplacer("<destination>", cell(name), "<url>", url)
	return "refused while adding a credential: " + cell(r.destination) + " · " + cell(r.reason) + " · last " + cell(r.at) + "\n" +
		"  " + say.Replace(words[0]) + "\n  do: " + say.Replace(words[1]) + "\n"
}

// patchAsked is what an agent does while production's patch is asked (EXPERIENCE.md D5).
const patchAsked = "Canyon is asking a person to approve its fix for production in Canyon's dashboard. Tell the user this, with the approval url. Do not promote or redeploy to apply the fix unless the user asks."

// aiEnv is one environment's line: its version, then what runs there, as envRow says it in words. A check-in is
// the same sentence a person reads, never its timestamp, and a warning says it is the platform's to act on.
func (a *app) aiEnv(label, flag string, e *env, absent string, now time.Time) {
	switch {
	case e == nil:
		a.printf("%s: %s\n", label, absent)
		return
	case e.running() == nil && e.Pending != nil:
		a.printf("%s: %s\n", label, strings.Join(slices.DeleteFunc([]string{a.version(e), e.Pending.line(), e.Pending.pulled()}, func(p string) bool { return p == "" }), " · "))
		return
	}
	parts := []string{a.version(e)}
	switch e.State {
	case "restarting":
		parts = append(parts, "keeps restarting"+e.restarts(), "canyon logs --env "+flag+" says why")
	case "stopped":
		parts = append(parts, "stopped", "canyon logs --env "+flag+" says why")
	case "running", "":
		if e.State == "running" {
			parts = append(parts, "running")
		}
		parts = append(parts, cell(e.URL))
		if e.DeployedAt != "" {
			parts = append(parts, "deployed "+cell(e.DeployedAt))
		}
		parts = append(parts, e.Answered.flaggedWords()) // Canyon's words; why nothing answered, the app's, follows under appSays
		if e.patched() {
			parts = append(parts, strings.TrimSpace("patched by Canyon "+cell(e.Patch.Since)))
		}
	default:
		parts = append(parts, cell(e.URL), "couldn't check whether it runs", e.Answered.flaggedWords())
	}
	if line, warn := a.checkIn(e, now); warn {
		parts = append(parts, line+" (your platform's pods: nothing to do in Canyon)")
	} else {
		parts = append(parts, line)
	}
	if e.Pending != nil {
		parts = append(parts, e.Pending.line(), e.Pending.pulled())
	}
	a.printf("%s: %s\n", label, strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), " · ")) // a cell the read does not carry, as row skips it
	if e.Answered.flaggedWords() != "" && e.Answered.Error != "" {
		a.appLines(e.Answered.Error)
	}
}
