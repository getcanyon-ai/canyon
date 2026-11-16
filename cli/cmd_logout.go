package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

// runLogout is §8.2's thirteenth verb, and not an rm: DELETE /api/v1/sessions first, so the session
// stops working everywhere and not just on this disk, and the file second. The host stays, so the
// next `canyon login` offers it. The session it revokes is the one the file holds, at the host the
// file holds: --host or CANYON_HOST would send the delete elsewhere, with nothing in it. A <url>
// names another host to sign out of, in the canonical form; one held in hosts/ loses its file even
// when its host can't be reached, and that is said.
func runLogout(ctx context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		fs.Lookup("host").Usage = "" // refused below, so never offered: a <url> names another host
	})
	if err != nil {
		return a.usage(err)
	}
	named := ""
	switch {
	case a.hostFlag != "":
		return a.usage(errors.New("logout takes no --host: it signs out of the host canyon login saved, or the one its <url> names"))
	case len(rest) > 1:
		return a.usage(fmt.Errorf("logout takes one host, and got %q as well", rest[1]))
	case len(rest) == 1:
		if named, err = validHost("the host you named", rest[0]); err != nil {
			return a.usage(err)
		}
	}
	c := a.config()
	if err := a.dropStale(ctx, c); err != nil { // each distinct session removed is revoked, a stale one for this host's included
		return a.fail(err)
	}
	if named != "" && named != c.Host {
		return a.logoutOf(ctx, named)
	}
	if c.Host == "" || c.Session == "" {
		if named != "" {
			a.printf("\n  This machine is not signed in to %s.\n", named)
			return exitOK
		}
		a.printf("\n  This machine is not signed in.\n\n  canyon login %s\n", a.suggested())
		return exitOK
	}
	answer := revoke(ctx, c)
	if answer != nil && !alreadyEnded(answer) {
		return a.fail(answer) // an unreachable host keeps the session live, and the file keeps it too: run it again
	}
	next := &config{Host: c.Host, Public: c.Public}
	if err := next.save(a.getenv); err != nil {
		return a.fail(err)
	}
	a.conf = next // memory mirrors the file, as remember() keeps it: nothing after this reads a session the host has ended
	a.signedOut(c.Host, answer != nil)
	return exitOK
}

// logoutOf signs out of a host config.json does not name: its hosts/ file goes whatever the host
// answers, since nothing else on this machine would ever revoke it. A host that couldn't be reached
// is said to keep the session, and exits 0: this machine is signed out of it. Any other answer is the
// host's own words, and exits 1.
func (a *app) logoutOf(ctx context.Context, host string) int {
	held, answer, err := a.endHostFile(ctx, host, "")
	switch {
	case err != nil:
		return a.fail(err)
	case !held:
		a.printf("\n  This machine is not signed in to %s.\n", host)
		return exitOK
	case answer == nil, alreadyEnded(answer):
		a.signedOut(host, answer != nil)
		return exitOK
	case !unanswered(answer):
		return a.fail(answer)
	}
	a.letGo(host, answer)
	return exitOK
}

// dropStale revokes and removes the hosts/ file for the host config.json names (cli.md §8.4). An
// older CLI signed in there again, or signed out and left it: no verb offers it, since config.json is
// authoritative for its host, but the host still honours it. One that holds config.json's own
// session is removed and not revoked. What the host answered is said; the error is only a file that
// could not be removed.
func (a *app) dropStale(ctx context.Context, current *config) error {
	_, answer, err := a.endHostFile(ctx, current.Host, current.Session)
	a.letGo(current.Host, answer)
	return err
}

// endHostFile removes host's file in hosts/, revoking the session in it first unless that is keep.
// A file refused for its mode alone is revoked too — ending a session is not trusting it — and any
// refused file is removed with a line saying so. held says whether there was a session to end, and
// answer is what its host said; err is a file that could not be removed.
func (a *app) endHostFile(ctx context.Context, host, keep string) (held bool, answer, err error) {
	path, name, refused := hostPath(a.getenv, host)
	if refused != nil {
		a.ignore(refused) // a hosts/ this CLI will not read is not one it removes from either
	}
	if refused != nil || path == "" { // a host that gets no file, config.json's "" among them
		return false, nil, nil
	}
	c, refused := readSession(path, name)
	if refused != nil {
		a.errf("\n  %s %s: %s, so it is removed.\n", a.render.glyph(stateWarn), path, refused.why)
	}
	if c != nil && c.Session != "" && c.Session != keep {
		held, answer = true, revoke(ctx, c)
	}
	return held, answer, removeFile(path)
}

// revoke asks a session's host to end it, DELETE /api/v1/sessions, under the cookie name its public
// URL gives it.
func revoke(ctx context.Context, c *config) error {
	client := newClient(c.Host)
	client.Public = c.Public // before the seed: it names the cookie
	client.seed(c.Session)
	return client.del(ctx, "/api/v1/sessions")
}

// alreadyEnded is a host's 401 to a revoke: it had ended the session itself, and nothing is left to.
func alreadyEnded(answer error) bool {
	var expired *SessionExpired
	return errors.As(answer, &expired)
}

// removeFile removes a hosts/ file, and a file already gone is removed.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (a *app) signedOut(host string, dead bool) {
	note := ""
	if dead {
		note = " (that session had already expired)"
	}
	a.printf("\n  %s Signed out of %s%s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), host, note)
}

// letGo says when a session this machine has let go of may still work at its host: one that
// couldn't be reached keeps it until it expires, and any other refusal is the host's own words, as is
// a file that could not be removed.
func (a *app) letGo(host string, answer error) {
	var api *APIError
	switch {
	case answer == nil, alreadyEnded(answer):
	case unanswered(answer):
		a.errf("\n  %s Removed this machine's session for %s, but %s couldn't be reached to end it: it works until it expires or an admin revokes it.\n", a.render.glyph(stateWarn), host, host)
	case errors.As(answer, &api):
		a.errf("\n  %s %s\n", a.render.glyph(stateWarn), api.Message)
	default:
		a.errf("\n  %s %s\n", a.render.glyph(stateWarn), answer)
	}
}
