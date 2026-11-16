package db

import (
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/getcanyon-ai/canyon/internal/pgtest"
)

// The migration that records whether a front door passed the probe (#2438) marks the stored door probed only on a
// host that never saved a platform webhook: such a host never ran under run_by: org, so every door it saved passed the
// probe. On a host that saved one — whether it hands apps off now or switched back — a door may have been saved under
// org, where nothing was probed: it starts not probed, and Canyon runs no app behind it until it is saved again.
func TestTheProbedBackfillMarksADoorProbedOnlyWhereNoWebhookWasSaved(t *testing.T) {
	const probed = "migrations/0105_front_door_probed_release_attempts.sql"
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || !slices.Contains(names, probed) {
		t.Fatalf("%s among %d migrations (%v)", probed, len(names), err)
	}
	slices.Sort(names)
	for _, c := range []struct {
		name, runBy string
		webhook     bool
	}{{"canyon, no webhook", "canyon", false}, {"canyon, a webhook saved", "canyon", true}, {"org", "org", true}} {
		t.Run(c.name, func(t *testing.T) {
			p, ctx := pgtest.Schema(t)
			upTo := fstest.MapFS{}
			add := func(name string) {
				data, err := migrations.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				upTo[name] = &fstest.MapFile{Data: data}
			}
			for _, name := range names[:slices.Index(names, probed)] {
				add(name)
			}
			if _, err := migrate(ctx, p, upTo); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Exec(ctx, "UPDATE org_preferences SET run_by = $1", c.runBy); err != nil {
				t.Fatal(err)
			}
			if _, err := p.Exec(ctx, `INSERT INTO front_doors (door, set_by_kind, set_by_subject, set_by_display) VALUES ('{"choice": 1}', 'person', 'dana', 'Dana')`); err != nil {
				t.Fatal(err)
			}
			if c.webhook {
				if _, err := p.Exec(ctx, `INSERT INTO platform_webhook (id, url, key_ref, sealed_key, sealed_value, token_hash, set_by_kind, set_by_subject, set_by_display)
					VALUES (1, 'https://deploy.meridian.example/hooks', 1, '', '', '', 'person', 'dana', 'Dana')`); err != nil {
					t.Fatal(err)
				}
			}
			add(probed)
			if _, err := migrate(ctx, p, upTo); err != nil {
				t.Fatal(err)
			}
			var setAt time.Time
			var probedAt *time.Time
			if err := p.QueryRow(ctx, "SELECT set_at, probed_at FROM front_doors").Scan(&setAt, &probedAt); err != nil {
				t.Fatal(err)
			}
			if !c.webhook && (probedAt == nil || !probedAt.Equal(setAt)) || c.webhook && probedAt != nil {
				t.Fatalf("%s: the door's probed_at is %v, its set_at %v", c.name, probedAt, setAt)
			}
		})
	}
}
