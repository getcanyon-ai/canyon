package cli

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// tourFiles is what `canyon demo tour plant` writes, three times: the claims tracker the quick way, with
// a page of its own making (tour/app); that page again in the org's design system (tour/design);
// and a search that builds its SQL from the request (tour/search). The code the tour's reviews stop
// comes from here, never from a host's seed (cli.md §6.14.1). They live under testdata so that no
// scanner or dependency bot reads their package.json as Canyon's own (#2861).
//
//go:embed testdata/tour
var tourFiles embed.FS

// tourDir is the directory plant writes, beside wherever the tour was started.
const tourDir = "canyon-tour"

// The chapters, in order. tourEnd is the recap: it plants nothing and nothing follows it.
const (
	tourFirst    = 1 // the org's standards: the design system
	tourSecurity = 2 // the search that builds its SQL from the request
	tourProd     = 3 // production's rules
	tourEnd      = 4
)

// What Planted counts: the plants made so far, in the order the chapters make them.
const (
	plantedApp    = 1
	plantedDesign = 2
	plantedSearch = 3
)

// tourState is the visitor's place, in tour.json beside config.json: an agent that starts a new
// session, or a person who comes back tomorrow, is handed the chapter they were on.
type tourState struct {
	Chapter int    `json:"chapter"`
	Dir     string `json:"dir,omitempty"`     // where plant wrote the app, absolute
	Planted int    `json:"planted,omitempty"` // plantedApp, plantedDesign or plantedSearch: the last plant made
	Base    string `json:"base,omitempty"`    // what preview ran when the search was planted
	Blocked int    `json:"blocked,omitempty"` // the reviews Canyon had blocked when the search was planted

}

func tourPath(getenv func(string) string) (string, error) {
	conf, err := configPath(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(conf), "tour.json"), nil
}

// loadTour answers chapter 1 for a machine with no tour.json, and for one whose file names a
// chapter this binary has none of: an older or newer CLI's place is not this one's.
func loadTour(getenv func(string) string) (tourState, error) {
	path, err := tourPath(getenv)
	if err != nil {
		return tourState{}, err
	}
	body, err := os.ReadFile(path) //nolint:gosec // this CLI's own file, beside its config
	if errors.Is(err, fs.ErrNotExist) {
		return tourState{Chapter: tourFirst}, nil
	}
	if err != nil {
		return tourState{}, fmt.Errorf("%q cannot be read: %s", path, plainly(err))
	}
	var s tourState
	if err := json.Unmarshal(body, &s); err != nil {
		return tourState{}, &tourUnreadable{path, err}
	}
	if s.Chapter < tourFirst || s.Chapter > tourEnd {
		return tourState{Chapter: tourFirst}, nil
	}
	return s, nil
}

// tourUnreadable is a tour.json that is there and is not the tour's place: said, never read as chapter 1.
type tourUnreadable struct {
	path string
	err  error
}

func (e *tourUnreadable) Error() string { return e.path + " is not the tour's place: " + e.err.Error() }

// tourRefused is a refusal of the tour's, said as a sentence: exit 1.
func (a *app) tourRefused(f string, v ...any) int {
	a.errf("\n  %s "+f+"\n", append([]any{a.render.glyph(stateFail)}, v...)...)
	return exitError
}

func (s tourState) save(getenv func(string) string) error {
	path, err := tourPath(getenv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicWrite(path, body, true)
}

// runTour is cli.md §6.14.1: the tour's chapters, one at a time. With no form it prints the chapter
// the visitor is on, and reaches no host: the text ships in the binary, as the skill's does.
func runTour(ctx context.Context, a *app, args []string) int {
	form := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		form, args = args[0], args[1:]
	}
	switch form {
	case "", "plant", "next", "skip", "end", "status", "reset":
	case "help":
		a.tourForms(a.out)
		return exitOK
	default:
		a.errf("\n  %s demo tour has no form %q\n", a.render.glyph(stateFail), form)
		a.tourForms(a.err)
		return exitError
	}
	if form != "" {
		a.form = "demo tour " + form
	}
	for _, arg := range args {
		if arg == "-h" || arg == "-help" || arg == "--help" { // asking how it works lists every form
			a.tourForms(a.out)
			return exitOK
		}
	}
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("%s takes no arguments, and got %q", cmpForm(form), rest[0]))
	}
	if form == "reset" { // it reads no place: it is how one that cannot be read starts over
		return a.tourReset()
	}
	state, err := loadTour(a.getenv)
	var unreadable *tourUnreadable
	if errors.As(err, &unreadable) {
		return a.tourRefused("The tour's saved place (%s) can't be read: %s. To start over: canyon demo tour reset", unreadable.path, plainly(unreadable.err))
	}
	if err != nil {
		return a.usage(err)
	}
	if form == "plant" || form == "next" {
		if code := a.tourOnDemo(ctx); code != exitOK {
			return code
		}
	}
	switch form {
	case "plant":
		return a.tourPlant(ctx, state)
	case "next":
		return a.tourNext(ctx, state)
	case "skip":
		return a.tourSkip(state)
	case "end":
		state.Chapter = tourEnd
		if err := state.save(a.getenv); err != nil {
			return a.usage(err)
		}
		a.printf("\n  %s Skipped to the end of the tour. Run: canyon demo tour\n", a.render.glyph(statePass))
		return exitOK
	case "status":
		return a.tourStatus(state)
	}
	a.printf("%s", a.tourChapter(state))
	return exitOK
}

// tourOnDemo refuses a host that is not the laptop demo: plant writes the tour's app for it and next
// reads the tour's app on it, and both belong on a Canyon on this machine with sign-in off. A host
// elsewhere is refused before anything is sent to it.
func (a *app) tourOnDemo(ctx context.Context) int {
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	why := ""
	if !loopback(client.Base) {
		why = "it's on another machine"
	} else if auth, err := firstContact(ctx, client); err != nil {
		return a.fail(err)
	} else if auth.Auth != "off" {
		why = "it signs people in"
	}
	if why == "" {
		return exitOK
	}
	a.errf("\n  %s The tour runs only on the demo on this machine, and %s isn't the demo: %s.\n\n  To start the demo: canyon demo up\n", a.render.glyph(stateFail), client.name(), why)
	return exitError
}

func cmpForm(form string) string {
	if form == "" {
		return "demo tour"
	}
	return "demo tour " + form
}

// tourForms is the forms this binary runs, as skillForms is skill's: `canyon --help` has one row.
func (a *app) tourForms(w io.Writer) {
	forms := [][2]string{
		{"canyon demo tour", "The chapter you are on: what it shows and what to do"},
		{"canyon demo tour plant", "Write this chapter's next files into ./" + tourDir},
		{"canyon demo tour next", "Go on to the next chapter, once this one is done"},
		{"canyon demo tour skip", "Go on to the next chapter without doing this one"},
		{"canyon demo tour end", "Skip to the end: the recap and where to go from here"},
		{"canyon demo tour status", "Which chapter you are on"},
		{"canyon demo tour reset", "Start the tour again from chapter 1"},
	}
	width := 0
	for _, f := range forms {
		width = max(width, len(f[0]))
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", a.render.paint(ansiBold, "USAGE"))
	for _, f := range forms {
		_, _ = fmt.Fprintf(w, "  %-*s    %s\n", width, f[0], f[1])
	}
}

// tourChapter is the chapter's text: the agent's instructions under a harness or --format ai, and
// the same chapter for a person reading along anywhere else.
func (a *app) tourChapter(s tourState) string {
	if a.render.Format == FormatAI {
		return tourAgent[s.Chapter]
	}
	return tourHuman[s.Chapter]
}

// tourPlant writes the chapter's next files and says what they are and that the tour wrote them.
// Chapter 1 plants twice: the app, into a directory that holds nothing else, and once Canyon has
// blocked it, its page again in the org's design system. Chapter 2 plants the search over that.
// A chapter reached by canyon demo tour skip plants what the skipped ones would have left as well, so
// each chapter stands alone: chapter 3 plants the finished app when preview runs none.
func (a *app) tourPlant(ctx context.Context, s tourState) int {
	if s.Chapter == tourEnd {
		return a.tourRefused("The tour is at its end, and the end plants nothing. To start again: canyon demo tour reset.")
	}
	fresh := !tourPlantedHere(s)
	if fresh {
		dir, err := filepath.Abs(tourDir)
		if err != nil {
			return a.usage(err)
		}
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			return a.tourRefused("./%s is here already, from an earlier tour or your own work, and the tour will not write over it. Run canyon demo tour plant from an empty folder, or remove ./%[1]s first.", tourDir)
		}
		s.Dir = dir
	}
	if s.Chapter == tourFirst && fresh {
		s.Planted = plantedApp
		return a.tourWrote(s, "PLANTED: the tour wrote this app, so every visitor starts from the same code. It is the quick version a coding agent often writes: public/index.html has colours, fonts and a list of its own making, and nothing from the org's design system.", "testdata/tour/app")
	}
	blocked, preview, code := a.tourSeen(ctx, s)
	if code != exitOK {
		return code
	}
	switch s.Chapter {
	case tourFirst:
		if blocked == 0 && preview != "" {
			return a.tourUnruled(s)
		}
		if blocked == 0 {
			return a.tourUndone(s, "Canyon has not reviewed the planted app yet. Run canyon deploy in ./"+tourDir+" before you change it")
		}
		s.Planted = plantedDesign
		return a.tourWrote(s, "The tour wrote the page again, in the org's design system: components.json names its registry, and public/canyon.css is its theme. In a real project the shadcn CLI fetches these from the registry; this demo's registry is not online, so the tour hands them over.", "testdata/tour/design")
	case tourSecurity:
		trees := []string{"testdata/tour/design", "testdata/tour/search"}
		if fresh {
			trees = append([]string{"testdata/tour/app"}, trees...)
		}
		s.Planted, s.Base, s.Blocked = plantedSearch, preview, blocked
		return a.tourWrote(s, "PLANTED: server.js now has a search that builds its SQL from the request. It is the quick version a coding agent often writes, put here on purpose so you can watch Canyon stop it.", trees...)
	}
	if preview != "" {
		return a.tourRefused("Chapter %d plants nothing: preview already runs the app it takes to production.", s.Chapter)
	}
	s.Planted = plantedDesign
	return a.tourWrote(s, "The tour wrote the finished claims tracker, in the org's design system, because the chapters that build it were skipped. It has no planted flaw.", "testdata/tour/app", "testdata/tour/design")
}

// tourSeen is what the host has seen of the tour's app: how many of its reviews ended blocked and
// the package preview runs, none of either for an app no deploy has made yet.
func (a *app) tourSeen(ctx context.Context, s tourState) (blocked int, preview string, code int) {
	p, err := readProject(s.Dir)
	var none *NoProjectFile
	if errors.As(err, &none) {
		return 0, "", exitOK
	}
	if err != nil {
		return 0, "", a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return 0, "", a.usage(err)
	}
	at := "/api/v1/projects/" + url.PathEscape(p.App)
	if blocked, err = tourBlocked(ctx, client, at); err != nil {
		return 0, "", a.fail(err)
	}
	var status statusRead
	if err := client.get(ctx, at+"/status", &status); err != nil {
		return 0, "", a.fail(err)
	}
	if env := status.Envs["preview"]; env != nil {
		preview = env.Package
	}
	return blocked, preview, exitOK
}

// tourSkip moves on without the chapter's work: the visitor's way past a chapter, where next is the
// agent's proof that one is done. What the next chapter then lacks, its plant writes.
func (a *app) tourSkip(s tourState) int {
	if s.Chapter == tourEnd {
		a.printf("\n  %s The tour is finished. canyon demo tour shows the recap; canyon demo tour reset starts it again.\n", a.render.glyph(statePass))
		return exitOK
	}
	s.Chapter, s.Base, s.Blocked = s.Chapter+1, "", 0
	if err := s.save(a.getenv); err != nil {
		return a.usage(err)
	}
	a.printf("\n  %s Skipped chapter %d. Run: canyon demo tour\n", a.render.glyph(statePass), s.Chapter-1)
	return exitOK
}

// tourPlantedHere is whether the tour's app is where the place says plant wrote it.
func tourPlantedHere(s tourState) bool {
	if s.Planted == 0 || s.Dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(s.Dir, "server.js"))
	return err == nil
}

// tourWrote writes the embedded trees into the tour's directory, a later one over an earlier, keeps
// the place and prints the note.
func (a *app) tourWrote(s tourState, note string, trees ...string) int {
	var wrote []string
	for _, from := range trees {
		names, err := tourWrite(from, s.Dir)
		if err != nil {
			return a.usage(err)
		}
		for _, name := range names {
			if !slices.Contains(wrote, name) {
				wrote = append(wrote, name)
			}
		}
	}
	slices.Sort(wrote)
	if err := s.save(a.getenv); err != nil {
		return a.usage(err)
	}
	a.planted(wrote, note)
	return exitOK
}

func (a *app) planted(wrote []string, note string) {
	a.printf("\n  %s Wrote %s into ./%s\n\n", a.render.glyph(statePass), plural(len(wrote), "file", "files"), tourDir)
	for _, name := range wrote {
		a.printf("    %s\n", name)
	}
	a.printf("\n  %s\n", note)
}

// tourWrite copies one embedded tree into dir, each file whole, and answers the names it wrote.
func tourWrite(from, dir string) ([]string, error) {
	var wrote []string
	err := fs.WalkDir(tourFiles, from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, filepath.FromSlash(path))
		if err != nil {
			return err
		}
		to := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o750)
		}
		body, err := tourFiles.ReadFile(path)
		if err != nil {
			return err
		}
		wrote = append(wrote, filepath.ToSlash(rel))
		return atomicWrite(to, body, false)
	})
	if err != nil {
		return nil, fmt.Errorf("./%s cannot be written: %s", tourDir, plainly(err))
	}
	return wrote, nil
}

// tourApp is the app the tour's directory deploys, and the client for its host. A directory with
// no canyon.toml has not been deployed: the first deploy writes one.
func (a *app) tourApp(s tourState) (project, *Client, int) {
	p, err := readProject(s.Dir)
	var none *NoProjectFile
	if errors.As(err, &none) {
		return p, nil, a.tourUndone(s, "the app in ./"+tourDir+" has not been deployed yet. Run canyon deploy there")
	}
	if err != nil {
		return p, nil, a.usage(err)
	}
	client, err := a.client()
	if err != nil {
		return p, nil, a.usage(err)
	}
	return p, client, exitOK
}

// tourUnruled is chapter 1 with an app Canyon passed and never blocked: the design-system rule was
// not in force when it was reviewed, so the block the chapter waits for cannot come. Only a person
// sets the rule, in the dashboard.
func (a *app) tourUnruled(s tourState) int {
	page := "the dashboard"
	if client, err := a.client(); err == nil {
		page = client.page()
	}
	return a.tourUndone(s, "Canyon passed the planted app, so the design-system rule isn't Blocking yet. "+
		"Set it at "+page+"/#/guardrails/policies?rule=design.org-system: \"Apps use the org's design system\" to Blocking, then Save. "+
		"Then run canyon demo tour reset, delete ./"+tourDir+", and start the chapter again")
}

// tourUndone refuses to move on, and says the one thing that is missing.
func (a *app) tourUndone(s tourState, missing string) int {
	a.errf("\n  %s Chapter %d isn't done: %s.\n", a.render.glyph(stateFail), s.Chapter, missing)
	return exitError
}

// tourNext moves to the next chapter once this one's work shows on the host, so a chapter cannot be
// skipped by saying so. Chapter 1 is done when Canyon has blocked the planted app and preview runs
// one that passed: a blocked deploy builds nothing. Chapter 2 is done when Canyon has blocked a
// review since the search was planted and preview runs a newer version than it did then. Chapter 3
// is done when production runs the app.
func (a *app) tourNext(ctx context.Context, s tourState) int {
	if s.Chapter == tourEnd {
		a.printf("\n  %s The tour is finished. canyon demo tour shows the recap; canyon demo tour reset starts it again.\n", a.render.glyph(statePass))
		return exitOK
	}
	if s.Dir == "" || s.Chapter == tourSecurity && s.Planted != plantedSearch {
		return a.tourUndone(s, "its files are not planted. Run canyon demo tour plant")
	}
	p, client, code := a.tourApp(s)
	if code != exitOK {
		return code
	}
	at := "/api/v1/projects/" + url.PathEscape(p.App)
	var status statusRead
	if err := client.get(ctx, at+"/status", &status); err != nil {
		return a.fail(err)
	}
	preview := status.Envs["preview"]
	live := preview != nil && preview.Package != ""
	switch s.Chapter {
	case tourFirst, tourSecurity:
		blocked, err := tourBlocked(ctx, client, at)
		if err != nil {
			return a.fail(err)
		}
		if blocked <= s.Blocked && s.Chapter == tourFirst && live {
			return a.tourUnruled(s)
		}
		if blocked <= s.Blocked {
			return a.tourUndone(s, "Canyon has not blocked what the tour planted yet. Run canyon deploy in ./"+tourDir+" before you change anything")
		}
		if !live || preview.Package == s.Base {
			return a.tourUndone(s, "preview does not run the fixed app yet. Fix what Canyon found, then run canyon deploy in ./"+tourDir)
		}
	default:
		if prod := status.Envs["prod"]; prod == nil || prod.Package == "" {
			return a.tourUndone(s, "the app is not in production yet. Run canyon deploy --env prod in ./"+tourDir)
		}
	}
	s.Chapter, s.Base, s.Blocked = s.Chapter+1, "", 0
	if err := s.save(a.getenv); err != nil {
		return a.usage(err)
	}
	a.printf("\n  %s Chapter %d is done. Run: canyon demo tour\n", a.render.glyph(statePass), s.Chapter-1)
	return exitOK
}

// tourBlocked is how many reviews of the app ended blocked: the runs list, a page at a time.
func tourBlocked(ctx context.Context, c *Client, at string) (int, error) {
	cursor, blocked := "", 0
	for {
		var page struct {
			Runs []runRow `json:"runs"`
			Next *string  `json:"next"`
		}
		if err := c.get(ctx, at+"/gate-runs?limit=200"+cursor, &page); err != nil {
			return 0, err
		}
		for _, r := range page.Runs {
			if r.State == "blocked" {
				blocked++
			}
		}
		if page.Next == nil {
			return blocked, nil
		}
		cursor = "&cursor=" + url.QueryEscape(*page.Next)
	}
}

func (a *app) tourStatus(s tourState) int {
	if s.Chapter == tourEnd {
		a.printf("\n  The tour is finished. canyon demo tour shows the recap; canyon demo tour reset starts it again.\n")
		return exitOK
	}
	planted := "not planted yet: canyon demo tour plant"
	if tourPlantedHere(s) {
		planted = "planted in " + cell(s.Dir)
	}
	a.printf("\n  Chapter %d of %d %s %s\n  The tour's app is %s\n", s.Chapter, tourEnd-1, a.dot(), tourTitles[s.Chapter], planted)
	return exitOK
}

// tourReset forgets the place and nothing else: the folder and the app on the host are the
// visitor's to keep or delete.
func (a *app) tourReset() int {
	path, err := tourPath(a.getenv)
	if err != nil {
		return a.usage(err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return a.tourRefused("The tour's saved place (%s) can't be removed: %s.", path, plainly(err))
	}
	a.printf("\n  %s The tour starts again at chapter 1. ./%s and its app in Canyon are left as they are. For a clean start, run canyon delete in ./%[2]s, then remove the folder.\n", a.render.glyph(statePass), tourDir)
	return exitOK
}
