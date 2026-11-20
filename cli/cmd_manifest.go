package cli

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type manifest struct {
	Host           string           `json:"host"`
	Policies       []manifestPolicy `json:"policies"`
	ComponentTypes []string         `json:"component_types"`
	Stacks         []stack          `json:"stacks"`
	DesignSystem   *designSystem    `json:"design_system"`
	ProdApproval   bool             `json:"prod_approval"` // a person approves production in the dashboard (contracts/routes.md)
	AppAdditions   []appAddition    `json:"app_additions"` // the app's own stricter states, asked for with ?app= (contracts/routes.md)
	App            string           `json:"-"`             // the app this directory's canyon.toml names, "" outside one
	Outbound       bool             `json:"-"`             // the host's via is canyon (GET /api/v1/egress): the agent's manifest says outbound
}

// viaCanyon is whether the host's via is canyon (GET /api/v1/egress, which every member reads), so the agent's manifest
// says outbound: where apps call out directly, Canyon refuses nothing. A read that fails, or doesn't answer within
// left, is no: it never fails the manifest, nor holds a session start past the hook's seconds.
func viaCanyon(ctx context.Context, c *Client, left time.Duration) bool {
	var host struct {
		Via string `json:"via"`
	}
	return left > 0 && c.do(ctx, http.MethodGet, "/api/v1/egress", "", nil, 0, nil, http.StatusOK, left, answerLimit, &host) == nil && host.Via == "canyon"
}

// appAddition is one of an app's additions to a rule: its own state in each gate it names, and who set it.
type appAddition struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	State        string   `json:"state"`
	Production   string   `json:"production"`
	SetBy        actorRef `json:"set_by"`
	How          string   `json:"how,omitempty"`          // how to pass a Canyon rule, where the addition blocks (contracts/routes.md)
	Instructions string   `json:"instructions,omitempty"` // a custom rule's text, where the addition blocks (contracts/routes.md)
	Says         string   `json:"says,omitempty"`         // beside it, on a rule checked by code, the condition Canyon checks
}

type manifestPolicy struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	State        string `json:"state"`
	Production   string `json:"production,omitempty"`   // its state before production when placed there (contracts/snapshot.md, Two gates)
	How          string `json:"how,omitempty"`          // how to pass a Canyon rule, Canyon's own line, where the rule blocks (contracts/routes.md)
	Instructions string `json:"instructions,omitempty"` // a custom rule's text, as its admin wrote it, where the rule blocks (contracts/routes.md)
	Says         string `json:"says,omitempty"`         // beside it, on a rule checked by code, the condition Canyon checks (#2639)
}

type stack struct {
	Language   string   `json:"language"`
	Frameworks []string `json:"frameworks"`
}

type designSystem struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	RegistryURL string `json:"registry_url"`
	TokensURL   string `json:"tokens_url"`
}

// runManifest prints what this host hands every agent in the org — for a person the dashboard's panel
// "What your agents are handed" as text, one value per line under the panel's own labels; for an agent
// (a session hook, --format ai) the same values written for it (agentManifest).
// Nothing is written anywhere: a harness runs the verb at session start and puts its stdout in
// the model's context, so a change on the dashboard reaches every agent at its next session,
// live. Under --harness the verb is a session hook, and a session hook never fails a session.
func runManifest(ctx context.Context, a *app, args []string) int {
	var harness string
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.StringVar(&harness, "harness", "", targetClaude+" | "+targetCodex+" | "+targetCursor+": print it the way that agent's session hook reads it")
	})
	// A known harness is a session hook, and a session hook never fails a session: whatever refuses
	// this run — a flag, a format the environment set, a host the config cannot use, the host
	// itself — is one line on stdout and exit 0, so the agent is told rather than the person.
	hook := harness == targetClaude || harness == targetCodex || harness == targetCursor
	switch {
	case err == nil && len(rest) > 0:
		err = fmt.Errorf("manifest takes no arguments, and got %q", rest[0])
	case err == nil && harness != "" && !hook:
		err = fmt.Errorf("--harness takes %s, %s or %s, the agents with a session hook", targetClaude, targetCodex, targetCursor)
	}
	var refreshed string
	if hook { // before anything else, a refused flag or format too: the file is the harness's, not the request's
		refreshed = a.refreshSkill(harness) + a.migrateHook(harness)
	}
	var client *Client
	if err == nil {
		client, err = a.client()
	}
	if err != nil {
		if !hook {
			return a.usage(err)
		}
		return a.emit(harness, refreshed+a.hookSays(err))
	}
	var read manifest
	at := "/api/v1/skill/manifest"
	if dir, werr := a.getwd(); werr == nil { // in an app's directory, its own rules too (cli.md §7.4); anywhere else, the org's
		if p, perr := readProject(dir); perr == nil {
			read.App, at = p.App, at+"?app="+url.QueryEscape(p.App)
		}
	}
	within := requestTimeout
	if hook { // a session start never waits on Canyon (#2703)
		within = hookWithin
	}
	began := a.clock.now()
	err = client.do(ctx, http.MethodGet, at, "", nil, 0, nil, http.StatusOK, within, answerLimit, &read)
	if err == nil && read.Host == "" { // a 200 of some other shape is some other service, not an org with nothing set: the host is the one field the contract never leaves empty
		err = &NotCanyon{Host: client.Base, Status: http.StatusOK}
	}
	switch {
	case err != nil && !hook:
		return a.fail(err)
	case err != nil:
		return a.emit(harness, refreshed+a.hookSays(err))
	case hook || a.render.Format == FormatAI: // an agent reads it: written for one (cli.md §7.4)
		read.Outbound = viaCanyon(ctx, client, within-a.clock.now().Sub(began)) // what is left of the hook's seconds
		return a.emit(harness, refreshed+agentManifest(read, hook))
	}
	return a.emit(harness, renderManifest(read))
}

// refreshSkill rewrites the harness's own skill file when its Canyon block is another canyon's text, so
// the agent reads the skill of the canyon it runs (cli.md §7.4), and answers the line that says so; ""
// when there is no such file, no block, or the block is already this binary's.
func (a *app) refreshSkill(harness string) string {
	if a.getenv("HOME") == "" { // ~/.claude would otherwise resolve under the working directory
		return ""
	}
	for _, t := range a.skillTargets() {
		if t.harness != harness || !staleBlock([]skillTarget{t}) {
			continue
		}
		if _, err := installSkill(t); err != nil {
			return "Canyon: your Canyon skill is from another canyon and was not updated: " + cell(err.Error()) + ". Run: canyon skill install\n"
		}
		return "Canyon updated its skill to the one canyon " + Version + " ships.\n"
	}
	return ""
}

// emit prints the text the way the harness reads it: bare, or inside Cursor's one-key envelope.
func (a *app) emit(harness, text string) int {
	if harness == targetCursor {
		envelope, _ := json.Marshal(struct {
			AdditionalContext string `json:"additional_context"`
		}{text}) // a string field, so the encoder cannot fail
		a.printf("%s\n", envelope)
		return exitOK
	}
	a.printf("%s", text)
	return exitOK
}

const (
	hookWithin    = 3 * time.Second // what a session hook gives the host: a session start never waits on Canyon (#2703)
	hookLineEvery = 24 * time.Hour
	hookLinesFile = "hook-lines.json"
)

// hookSays is what a session hook prints for err: hookLine's line, or nothing where that line is said
// once a day and was said within the last one.
func (a *app) hookSays(err error) string {
	line, daily := hookLine(err)
	if daily && !a.firstToday(line) {
		return ""
	}
	return line
}

// hookLine is what a session hook prints instead of §9's block, and whether it is said once a day
// (cli.md §7.4). An agent reads it at the start of a session that may have nothing to do with Canyon,
// so where the trouble is the host's or the session's — a host it cannot reach, refused, unresolved or
// silent past hookWithin; a session that has ended; an answer that is not Canyon's; TLS that fails —
// the line asks nothing of the agent until the user asks for Canyon, says what would bring Canyon back,
// and names the way to remove it, since a host that is gone may be one the person is done with. Those
// are said once a day: they are about the machine, not the session. A refusal in Canyon's own words,
// and a flag, format or host this run could not use, is the shipped error's sentence, every time.
func hookLine(err error) (line string, daily bool) {
	const idle, remove = " Nothing to do unless the user asks for Canyon", " To remove Canyon from your coding agents: canyon skill uninstall.\n"
	var unreachable *Unreachable
	var silent *NoAnswer
	var expired *SessionExpired
	var other *NotCanyon
	gone := func(host string) string {
		start := "start it"
		if loopback(host) {
			start += " (the demo: canyon demo up)"
		}
		return "Canyon: can't reach " + cell(host) + " (it may be down or gone)." + idle + ". To use it again, " + start + "." + remove
	}
	switch {
	case errors.As(err, &unreachable):
		return gone(unreachable.Host), true
	case errors.As(err, &silent):
		return gone(silent.Host), true
	case errors.As(err, &expired) && expired.Sent:
		return "Canyon: the session at " + cell(expired.Host) + " has expired." + idle + "; then they run canyon login " + cell(expired.Host) + "." + remove, true
	case errors.As(err, &expired):
		return "Canyon: this machine is not signed in to " + cell(expired.Host) + "." + idle + "; then they run canyon login " + cell(expired.Host) + "." + remove, true
	case errors.As(err, &other), tlsFailed(err):
		return "Canyon: " + cell(err.Error()) + "." + idle + "." + remove, true
	}
	return "Canyon: " + cell(err.Error()) + ".\n", false
}

// loopback is whether host is this machine, where a Canyon that is down is the demo, started with
// canyon demo up.
func loopback(host string) bool {
	u, err := url.Parse(host)
	return err == nil && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()).IsLoopback())
}

// tlsFailed is whether a request failed in TLS: a certificate this machine does not trust, a host
// that does not speak TLS where https names it, or an alert in the handshake.
func tlsFailed(err error) bool {
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	return errors.As(err, &verify) || errors.As(err, &record) || errors.As(err, &alert)
}

// firstToday is whether the session hook says line: not when it said this line within the last day.
// A line names its host and what is wrong there, so this is once a day per host and per trouble: a
// host that is gone would otherwise open every session in every project with it, and an agent in a
// session between learns it again from any verb it runs. A yes is recorded in hook-lines.json beside
// the config, with the lines said more than a day ago dropped. A record that cannot be read or
// written is no record: the line is said, as it was before there was one.
func (a *app) firstToday(line string) bool {
	path, err := configPath(a.getenv)
	if err != nil {
		return true
	}
	path = filepath.Join(filepath.Dir(path), hookLinesFile)
	var said map[string]time.Time
	if body, err := os.ReadFile(path); err != nil || json.Unmarshal(body, &said) != nil || said == nil { //nolint:gosec // beside this machine's own config, at a path composed from XDG_CONFIG_HOME or HOME and nothing else
		said = map[string]time.Time{}
	}
	now := a.clock.now()
	if ago := now.Sub(said[line]); ago >= 0 && ago < hookLineEvery {
		return false
	}
	for l, at := range said {
		if now.Sub(at) >= hookLineEvery {
			delete(said, l)
		}
	}
	said[line] = now
	if body, err := json.Marshal(said); err == nil && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = atomicWrite(path, body, true) // a write that fails says the line again next session, as before
	}
	return true
}

type section struct {
	label string
	lines []string
}

func (s *section) add(value string) {
	if value != "" {
		s.lines = append(s.lines, value)
	}
}

func renderManifest(m manifest) string {
	host := section{label: "Canyon host"}
	host.add(cell(m.Host))
	policies := section{label: "Policies"}
	for _, p := range m.Policies { // the dashboard's row: name, state, id
		policies.add(cell(p.Name) + "  " + cell(p.State) + "  " + cell(p.ID))
	}
	types := section{label: "Component types"}
	for _, t := range m.ComponentTypes {
		types.add(cell(t))
	}
	stacks := section{label: "Approved stacks"}
	for _, s := range m.Stacks {
		stacks.add(cell(s.Language))
		for _, f := range s.Frameworks {
			stacks.add("  " + cell(f))
		}
	}
	ds := section{label: "Design system"}
	if m.DesignSystem != nil {
		ds.add(cell(m.DesignSystem.Name) + "  " + cell(m.DesignSystem.Namespace))
		ds.add(cell(m.DesignSystem.RegistryURL))
		ds.add(cell(m.DesignSystem.TokensURL))
	}
	app := section{label: "This app (" + cell(m.App) + ")"}
	for _, add := range m.AppAdditions { // name, state, id, production's, who: the policies' row and what it adds
		line := []string{cell(add.Name)}
		if add.State != "" {
			line = append(line, cell(add.State))
		}
		line = append(line, cell(add.ID))
		if add.Production != "" {
			line = append(line, "production "+cell(add.Production))
		}
		app.add(strings.Join(append(line, "set by "+setter(add.SetBy)), "  "))
	}
	prod := section{label: "Production", lines: []string{"promoted from the CLI or the dashboard"}}
	if m.ProdApproval {
		prod.lines = []string{"approved by a person in the dashboard"}
	}
	var b strings.Builder
	b.WriteString("What your agents are handed\n")
	for _, s := range []section{host, policies, app, types, stacks, ds, prod} {
		if len(s.lines) == 0 {
			continue
		}
		b.WriteString("\n" + s.label + "\n")
		for _, line := range s.lines {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// appRules reads, for §5.2's blocked block, the app's additions that block before preview under
// which a blocking finding of this run falls: the blocks that are the app's, not the org's (cli.md
// §5.2). The host hands an addition's state only where it is stricter than the org's. A finding falls
// under a rule when its ruleId is the rule's or starts with it and a dot (contracts/finding.md). Under
// ai and on a blocked run alone; a read that fails is one line on stderr, and the verdict, the run's,
// prints without the lines.
func (a *app) appRules(ctx context.Context, client *Client, run *checkRun) {
	if a.render.Format != FormatAI || run.Result.State != "blocked" || approved(run.Result) {
		return
	}
	var m manifest
	if err := client.get(ctx, "/api/v1/skill/manifest?app="+url.QueryEscape(run.App), &m); err != nil {
		a.errf("\n  This app's own rules could not be read: %s\n", err.Error())
		return
	}
	for _, add := range m.AppAdditions {
		if add.State != "blocking" || slices.Contains(goLive, add.ID) { // a go-live rule's state is production's, never this gate's
			continue
		}
		under := func(f finding) bool {
			blocks, _ := f.Blocking()
			return blocks && (f.RuleID == add.ID || strings.HasPrefix(f.RuleID, add.ID+"."))
		}
		if slices.ContainsFunc(run.Result.Findings, under) {
			run.AppRules = append(run.AppRules, add)
		}
	}
}

// setter is who set an addition, by the name the dashboard shows (actorRef.name, #2705).
func setter(who actorRef) string {
	return cell(who.name())
}

// goLive are the go-live rules, never placed before preview: each one's state is its state before
// production (contracts/snapshot.md, Two gates).
var goLive = []string{"prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required", "platform.known-vuln", "prod.egress-allowed"}

// aiTwins are the AI review's rules that check a scanner rule's policy in the files no scanner rule of
// it reads, each with that scanner rule (gate/coverage.go's twins; contracts/finding.md, The AI review
// where no scanner reads the language).
var aiTwins = map[string]string{"ai.sql-string-concat": "sql.string-concat", "ai.code-injection": "code.injection"}

// foldTwin is scanner's policy with its AI twin's, each as the app holds it: the stricter state in each
// gate, and, when the two differ, one line saying each one's. A rule off before preview, or a state outside
// the contract — a production state is advisory or blocking, Off when absent (contracts/snapshot.md, Two
// gates) — is not folded: each keeps its own line.
func foldTwin(scanner, twin manifestPolicy) (manifestPolicy, string, bool) {
	rank := map[string]int{"advisory": 1, "blocking": 2} // an absent production is Off, below both
	for _, p := range []manifestPolicy{scanner, twin} {
		if _, known := rank[p.Production]; rank[p.State] == 0 || !known && p.Production != "" {
			return scanner, "", false
		}
	}
	stricter := func(a, b string) string {
		if rank[b] > rank[a] {
			return b
		}
		return a
	}
	folded := scanner
	folded.State, folded.Production = stricter(scanner.State, twin.State), stricter(scanner.Production, twin.Production)
	folded.How = cmp.Or(scanner.How, twin.How) // handed only where a rule blocks, so the stricter's
	if scanner.State == twin.State && scanner.Production == twin.Production {
		return folded, "", true
	}
	does := func(p manifestPolicy) string {
		said := map[string]string{"blocking": "blocks", "advisory": "only warns"}[p.State]
		if p.Production != "" && p.Production != p.State {
			said += ", and " + map[string]string{"blocking": "blocks production", "advisory": "only warns before production"}[p.Production]
		}
		return said
	}
	return folded, "In languages Canyon's scanners cover, it " + does(scanner) + "; in the rest only the AI review checks it, if one is set up, and it " + does(twin) + ".", true
}

// outboundHanded is what an agent's manifest says of outbound while the host's via is canyon (cli.md §7.4): the
// names refused in every mode are no one's to allow (design/EGRESS.md §3.3).
const outboundHanded = "Outbound: an app's environment may be under an allowlist, where Canyon refuses calls to any outside service not on it. Only a person on the app's team can allow a new one, in Canyon's dashboard; you cannot. Never try to reach a refused service another way. Canyon also refuses any call out to the internet to an IP address, a one-word host name or a port other than 80 and 443, and no one can allow it: call a host name on 80 or 443."

// secretsHanded is what an agent's manifest always says of secrets (cli.md §7.4, #2737).
const secretsHanded = "Secrets: never ask for or accept a secret's value in this conversation. When the app needs one, give the user canyon secret set NAME to run in their own terminal, which reads the value there."

// agentManifest is the manifest written for an agent, which a session hook puts in its context: the
// same values as the dashboard's panel, with no admin's heading and no rule ids, the policies grouped
// by what they do before preview, each saying what it does before production when that differs, and a
// first line saying this is canyon manifest's output, so the skill's "run canyon manifest" is already
// done. A policy off before preview and placed before production is grouped as production's alone.
// Under each Canyon rule the host handed a line on how to pass for, which it does for the ones that
// block alone, that line, once per rule; then, under their own heading, each custom rule the host
// handed text for, likewise: that text as the org's admin wrote it, one line of the agent's per line.
func agentManifest(m manifest, hook bool) string {
	told := map[string]bool{}
	howTo := func(id, how string) string {
		if how == "" || told[id] {
			return ""
		}
		told[id] = true
		return "\n  To pass: " + cell(how)
	}
	var own []string
	written := map[string]bool{}
	ownRule := func(id, name, text, says string) {
		if text == "" || written[id] {
			return
		}
		written[id] = true
		rule := "- " + cell(name)
		for _, l := range strings.Split(text, "\n") { // every line indented under the rule's, so no line of it reads as a heading of Canyon's
			rule += "\n  " + cell(l)
		}
		if says != "" { // what Canyon checks by code: the agent builds to it
			rule += "\n  Canyon checks this by code: " + cell(says)
		}
		own = append(own, rule)
	}
	var b strings.Builder
	if hook { // the session hook's: the agent has them before it asks
		b.WriteString("Canyon's settings for this org, as canyon manifest prints them. They are in this session already: you need not run canyon manifest.\n")
	} else {
		b.WriteString("Canyon's settings for this org.\n")
	}
	b.WriteString("\nCanyon host: " + cell(m.Host) + "\n")
	heading := map[string]string{"blocking": "Policies that block:", "advisory": "Policies that only warn:", "off": "Policies for production only:"}
	prod := map[string]string{"blocking": "blocks production", "advisory": "only warns before production"}
	var states []string
	byState := map[string][]string{}
	policies, twinNote := slices.Clone(m.Policies), map[string]string{}
	held := func(p manifestPolicy) manifestPolicy { // as this app holds it: its addition's states, where stricter (contracts/routes.md, app_additions)
		for _, add := range m.AppAdditions {
			if add.ID == p.ID {
				p.State, p.Production, p.How = cmp.Or(add.State, p.State), cmp.Or(add.Production, p.Production), cmp.Or(add.How, p.How)
			}
		}
		return p
	}
	for _, twin := range m.Policies { // one policy, one line: listed apart, it both blocked and only warned (#2694)
		scanner, ok := aiTwins[twin.ID]
		i := slices.IndexFunc(policies, func(p manifestPolicy) bool { return p.ID == scanner })
		if !ok || i < 0 {
			continue
		}
		folded, note, ok := foldTwin(held(policies[i]), held(twin))
		if !ok {
			continue
		}
		policies[i], twinNote[scanner] = folded, note
		policies = slices.DeleteFunc(policies, func(p manifestPolicy) bool { return p.ID == twin.ID })
	}
	for _, p := range policies {
		if p.ID == "prod.human-approval" { // the last line says it for the agent: its name alone had agents say a person promotes, then promote (QA round 3)
			continue
		}
		if slices.Contains(goLive, p.ID) { // never placed before preview: its state is production's (contracts/snapshot.md, Two gates)
			p.State, p.Production = "off", p.State
		}
		line := "- " + cell(p.Name)
		switch {
		case p.State == "off" && p.Production == "": // off in both gates: the host hands none out
			continue
		case p.State == "off":
			line += " (" + cmp.Or(map[string]string{"blocking": "blocks", "advisory": "only warns"}[p.Production], cell(p.Production)) + ")"
		case p.Production != "" && p.Production != p.State:
			line += " (" + cmp.Or(prod[p.Production], cell(p.Production)+" before production") + ")"
		}
		line += howTo(p.ID, p.How)
		if note := twinNote[p.ID]; note != "" {
			line += "\n  " + note
		}
		ownRule(p.ID, p.Name, p.Instructions, p.Says)
		if _, seen := byState[p.State]; !seen {
			states = append(states, p.State)
		}
		byState[p.State] = append(byState[p.State], line)
	}
	order := map[string]int{"blocking": -2, "advisory": -1, "off": 1} // a fixed order, whichever rule comes first; a state this build does not know between
	slices.SortStableFunc(states, func(a, b string) int { return order[a] - order[b] })
	for _, state := range states {
		label := heading[state]
		if label == "" { // a state this build does not know is said as the host sent it, never dropped
			label = "Policies (" + cell(state) + "):"
		}
		b.WriteString("\n" + label + "\n" + strings.Join(byState[state], "\n") + "\n")
	}
	for _, add := range m.AppAdditions {
		ownRule(add.ID, add.Name, add.Instructions, add.Says)
	}
	if len(own) > 0 {
		b.WriteString("\nYour org's own rules (written by its admins):\n" + strings.Join(own, "\n") + "\n")
	}
	if len(m.AppAdditions) > 0 {
		b.WriteString("\nThis app (" + cell(m.App) + ") holds these stricter than the org's, set by its team:\n")
		for _, add := range m.AppAdditions {
			state, production := add.State, add.Production
			if slices.Contains(goLive, add.ID) { // a go-live rule's state is production's
				state, production = "", add.State
			}
			var what []string // a state this build does not know is said as the host sent it, as the policies' are
			if state != "" {
				what = append(what, cmp.Or(map[string]string{"blocking": "blocks", "advisory": "only warns"}[state], cell(state)))
			}
			if production != "" {
				what = append(what, cmp.Or(prod[production], cell(production)+" before production"))
			}
			b.WriteString("- " + cell(add.Name) + ": " + strings.Join(what, ", ") + " (set by " + setter(add.SetBy) + ")" + howTo(add.ID, add.How) + "\n")
		}
	}
	if len(m.ComponentTypes) > 0 {
		types := make([]string, len(m.ComponentTypes))
		for i, t := range m.ComponentTypes {
			types[i] = cell(t)
		}
		b.WriteString("\nResources an app can declare in canyon.toml: " + strings.Join(types, ", ") + "\n")
	}
	if len(m.Stacks) > 0 {
		b.WriteString("\nApproved stacks:\n")
		for _, s := range m.Stacks {
			line := "- " + cell(s.Language)
			if len(s.Frameworks) > 0 {
				frameworks := make([]string, len(s.Frameworks))
				for i, f := range s.Frameworks {
					frameworks[i] = cell(f)
				}
				line += ": " + strings.Join(frameworks, ", ")
			}
			b.WriteString(line + "\n")
		}
	}
	if ds := m.DesignSystem; ds != nil {
		b.WriteString("\nDesign system: " + cell(ds.Name) + " (" + cell(ds.Namespace) + ")\n")
		for _, l := range [][2]string{{"registry", ds.RegistryURL}, {"tokens", ds.TokensURL}} {
			if l[1] != "" {
				b.WriteString("- " + l[0] + ": " + cell(l[1]) + "\n")
			}
		}
	}
	// Once, where apps' calls go through Canyon: an agent adding a call to a new outside service learned only after its
	// deploy that a person allows it, and agents never widen it (design/PERMISSIONS.md, Outbound; cli.md §7.4, #2708).
	if m.Outbound {
		b.WriteString("\n" + outboundHanded + "\n")
	}
	// Always said: a value pasted into a chat is in the agent's transcript and its provider's logs, and canyon secret set
	// reads it in the person's own terminal (cli.md §7.4, #2737).
	b.WriteString("\n" + secretsHanded + "\n")
	// Always said, so an agent never guesses whether its promotion waits for a person (cli.md §7.4).
	if m.ProdApproval {
		b.WriteString("\nProduction: a person on the app's team approves it in Canyon's dashboard. When the user asks for production, you run canyon deploy --env prod; Canyon answers WAITING FOR APPROVAL with a link: give the link to the user and stop. Never open or approve it yourself.\n")
	} else {
		b.WriteString("\nProduction: when the user asks for production, you run canyon deploy --env prod (add --yes only when they asked in this conversation). It promotes; nobody else approves it.\n")
	}
	return b.String()
}
