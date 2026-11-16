package patching

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/runs"
)

// waveSizes are a platform identity's waves (design/PATCHING.md §4.2, decided fixed in v1, §14.3): the
// canary, one app's preview, then 5, 25 and the rest.
var waveSizes = [...]int{1, 5, 25}

const lastWave = len(waveSizes)

// waveApp is one app of an identity: the wave that took it, -1 for none yet, what became of its patch, and
// whether a watch of it ended early, after which it is never the canary.
type waveApp struct {
	Project  string
	Wave     int
	Outcome  string
	NoCanary bool
}

// order is the canary order: a hash of the app's id, so the canary is stable across ticks (§4.2).
func order(project string) string {
	sum := sha256.Sum256([]byte(project))
	return hex.EncodeToString(sum[:])
}

// advance is a tick's move of an identity's waves (design/PATCHING.md §4.2): the wave it is in after the
// tick, and the wave each app not yet in one is taken into. A paused or stalled identity moves nothing. The canary's
// wave waits for one watch that ran to its end: while none did and none is under way, the next app in
// canary order with a patch planned is a replacement canary, never one a watch of which ended early: planned
// again, such an app is taken by a later wave. A later wave is done once every app of it ended, those that proved nothing left out of
// its count; a stalled identity takes no next wave, and none starts while no app waits for it, so an app
// planned on a later tick is taken by the wave next in line, never straight into the rest. The last wave
// takes every app planned since.
func advance(wave int, paused, stalled bool, apps []waveApp) (int, map[string]int) {
	taken := map[string]int{}
	if paused {
		return wave, taken
	}
	var waiting []string // planned, in no wave yet, in canary order
	never := map[string]bool{}
	for _, a := range apps {
		if a.Wave < 0 && a.Outcome == runs.PatchPlanned {
			waiting = append(waiting, a.Project)
			never[a.Project] = a.NoCanary
		}
	}
	slices.SortFunc(waiting, func(x, y string) int { return cmp.Compare(order(x), order(y)) })
	take := func(w, n int) { // n of them, every one when n is negative
		for ; len(waiting) > 0 && n != 0; n-- {
			taken[waiting[0]], waiting = w, waiting[1:]
		}
	}
	canary := func() { // the first in canary order that may be one
		if i := slices.IndexFunc(waiting, func(p string) bool { return !never[p] }); i >= 0 {
			taken[waiting[i]] = 0
			waiting = slices.Delete(waiting, i, i+1)
		}
	}
	for {
		passed, underWay := false, false
		for _, a := range apps {
			if w, now := taken[a.Project]; a.Wave == wave || now && w == wave {
				passed = passed || a.Outcome == runs.PatchPassed
				underWay = underWay || a.Outcome == runs.PatchPlanned || a.Outcome == runs.PatchRunning
			}
		}
		switch {
		case stalled:
			return wave, taken
		case wave == 0 && !passed && !underWay:
			canary() // the canary, or its replacement
			return wave, taken
		case wave == lastWave:
			take(lastWave, -1)
			return wave, taken
		case underWay, len(waiting) == 0: // a wave starts only with apps to take: one planned later is taken by the next
			return wave, taken
		}
		wave++
		if wave < lastWave {
			take(wave, waveSizes[wave])
		}
	}
}

// moveWaves is the tick's advance of identity in tx, the identity's row and its apps' locked: the wave it is
// in, and each app a wave took.
func moveWaves(ctx context.Context, tx pgx.Tx, identity string) error {
	var wave int
	var paused, stalled bool
	if err := tx.QueryRow(ctx, "SELECT wave, paused_at IS NOT NULL, stalled_at IS NOT NULL FROM patch_identities WHERE id = $1 FOR UPDATE", identity).
		Scan(&wave, &paused, &stalled); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT project_id, coalesce(wave, -1), outcome, no_canary FROM patch_wave_apps WHERE identity = $1 FOR UPDATE", identity)
	if err != nil {
		return err
	}
	apps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[waveApp])
	if err != nil {
		return err
	}
	moved, taken := advance(wave, paused, stalled, apps)
	if _, err := tx.Exec(ctx, "UPDATE patch_identities SET wave = $2 WHERE id = $1", identity, moved); err != nil {
		return err
	}
	for project, w := range taken {
		if _, err := tx.Exec(ctx, "UPDATE patch_wave_apps SET wave = $3 WHERE identity = $1 AND project_id = $2", identity, project, w); err != nil {
			return err
		}
	}
	return nil
}
