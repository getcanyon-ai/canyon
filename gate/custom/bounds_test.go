//go:build !race

// The tests that measure expressions at the facts' bounds, which make test-bounds runs without -race:
// the race detector makes CEL's cost tracker some fifteen times slower and its allocations no measure.

package custom

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// Every function of the environment holds at the facts' bounds: called on every file, every
// dependency or every secret at its largest, with CEL's tracker on, it costs no more than estimated and
// allocates no more than MaxAlloc. A function added to the environment fails here until it has its
// case, and one Canyon refuses needs none.
func TestEveryFunctionHoldsAtTheBounds(t *testing.T) {
	cases := map[string]string{
		"_+_":             `files.exists(f, (f + f + f).size() == 0)`,
		"bool":            `bool('true')`,
		"bytes":           `files.exists(f, bytes(f).size() == 0)`,
		"contains":        `files.exists(f, f.contains('#'))`,
		"double":          `double('1.5') > 1.0`,
		"duration":        `duration('1h') > duration('1m')`,
		"dyn":             `dyn(files).size() > 0`,
		"endsWith":        `files.exists(f, f.endsWith('#'))`,
		"getDate":         `timestamp('2026-11-03T00:00:00Z').getDate() == 3`,
		"getDayOfMonth":   `timestamp('2026-11-03T00:00:00Z').getDayOfMonth() == 2`,
		"getDayOfWeek":    `timestamp('2026-11-03T00:00:00Z').getDayOfWeek() == 2`,
		"getDayOfYear":    `timestamp('2026-11-03T00:00:00Z').getDayOfYear() > 0`,
		"getFullYear":     `timestamp('2026-11-03T00:00:00Z').getFullYear() == 2026`,
		"getHours":        `timestamp('2026-11-03T00:00:00Z').getHours() == 0`,
		"getMilliseconds": `timestamp('2026-11-03T00:00:00Z').getMilliseconds() == 0`,
		"getMinutes":      `timestamp('2026-11-03T00:00:00Z').getMinutes() == 0`,
		"getMonth":        `timestamp('2026-11-03T00:00:00Z').getMonth() == 10`,
		"getSeconds":      `timestamp('2026-11-03T00:00:00Z').getSeconds() == 0`,
		"in":              `files.exists(f, f in roles)`,
		"int":             `files.exists(f, int(f.size()) == 0)`,
		"lowerAscii":      `files.exists(f, f.lowerAscii() == '#')`,
		"matches":         `files.exists(f, f.matches('#$'))`,
		"reverse":         `files.exists(f, f.reverse() == '#')`,
		"size":            `size(files) > 0`,
		"startsWith":      `files.exists(f, f.startsWith('#'))`,
		"string":          `files.exists(f, string(f.size()) == '#')`,
		"substring":       `files.exists(f, f.substring(1) == '#')`,
		"timestamp":       `timestamp('2026-11-03T00:00:00Z') > timestamp('2026-01-01T00:00:00Z')`,
		"trim":            `files.exists(f, f.trim() == '#')`,

		"uint":           `files.exists(f, uint(f.size()) == 0u)`,
		"upperAscii":     `files.exists(f, f.upperAscii() == '#')`,
		"versionAtLeast": `dependencies.exists(d, versionAtLeast('1.2.3', '1.0.0') && d.name == '#')`,
	}
	e, err := env()
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range e.Functions() {
		name := fn.Name()
		if _, ok := cases[name]; !ok && !strings.ContainsAny(name, "_@!") && !slices.Contains(unbounded, name) {
			t.Errorf("%s has no case", name)
		}
	}
	for name, expr := range cases {
		p, err := Compile(expr)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if why := held(p); why != "" {
			t.Errorf("%s: %s", name, why)
		}
	}
}

// shapes are expressions that grow with k, one for each way an expression can spend: concatenation,
// conversions, scans, regular expressions, lists of lists, comprehensions inside comprehensions, and
// the functions whose output can outgrow their input.
var shapes = map[string]func(k int) string{
	"concat":     func(k int) string { return `files.exists(f, (f` + strings.Repeat(" + f", k) + `).size() == 0)` },
	"map concat": func(k int) string { return `files.map(f, f` + strings.Repeat(" + f", k) + `).size() == 0` },
	"lowerAscii": func(k int) string { return `files.exists(f, f` + strings.Repeat(".lowerAscii()", k) + ` == '#')` },
	"scans": func(k int) string {
		return `files.exists(f, f.endsWith('#')` + strings.Repeat(" || f.contains('#')", k) + `)`
	},
	"matches": func(k int) string {
		return `files.exists(f, f.matches('#$')` + strings.Repeat(" || f.matches('a#')", k) + `)`
	},
	"lists of maps": func(k int) string {
		return `[` + strings.TrimSuffix(strings.Repeat("files.map(f, f.upperAscii()), ", k+1), ", ") + `].exists(l, l.size() == 0)`
	},
	"nested secrets": func(k int) string {
		return `secrets.all(s, secrets.exists(p, p.name == s.name` + strings.Repeat(" && p.env.startsWith(s.where)", k) + `))`
	},
	"dependency pairs": func(k int) string {
		return `dependencies.exists(d, d.name + d.version` + strings.Repeat(" + d.ecosystem", k) + ` == '#')`
	},
	"replace": func(k int) string { return `files.join('')` + strings.Repeat(".replace('a', 'aa')", k) + `.size() > 0` },
	"join": func(k int) string {
		return `[` + strings.TrimSuffix(strings.Repeat("files.join(files[0]), ", k), ", ") + `].exists(s, s.size() == 0)`
	},
	"filter": func(k int) string {
		return `files.filter(f, f.size() > 1` + strings.Repeat(" && f.size() > 1", k) + `).size() == 0`
	},
	"list in a map": func(k int) string { return `files.map(f, [f` + strings.Repeat(", f", k) + `]).size() == 0` },
	"indexOf": func(k int) string {
		return `files.exists(f, f.indexOf('#') > 0` + strings.Repeat(" || f.indexOf('#') > 0", k) + `)`
	},
	"lastIndexOf": func(k int) string {
		return `files.exists(f, f.lastIndexOf('#') > 0` + strings.Repeat(" || f.lastIndexOf('#') > 0", k) + `)`
	},
	"charAt": func(k int) string {
		return `files.exists(f, f.charAt(1) == '#'` + strings.Repeat(" || f.charAt(1) == '#'", k) + `)`
	},
	"split": func(k int) string {
		return `files.exists(f, f.split('')` + strings.Repeat(".size() > 0 && f.split('')", k) + `.exists(c, c == '#'))`
	},
}

// held is why p does not hold at the facts' bounds within EvalDeadline, "" when it does.
func held(p *Program) string {
	ctx, cancel := context.WithTimeout(context.Background(), EvalDeadline)
	defer cancel()
	return p.AtTheBounds(ctx)
}

// slow are the shapes whose largest accepted expression runs past admitDeadline at the bounds, cheap
// as CEL estimates it: the tracker's time grows with the square of a list's length (#2638 review).
var slow = []string{"filter", "list in a map", "matches", "scans"}

// The bounds hold for the largest expression of every shape Compile accepts: admitted, it costs no
// more than its estimate at the facts' bounds, allocates no more than MaxAlloc and finishes inside
// EvalDeadline; or Admit refuses it, as it must the slow ones. A shape none of whose expressions
// compiles holds by refusal (#2638 review).
func TestTheLargestAcceptedExpressionOfEveryShapeHoldsAtTheBounds(t *testing.T) {
	for name, shape := range shapes {
		var largest *Program
		k := 0
		for n := 1; n <= 640; n++ {
			p, err := Compile(shape(n))
			if err != nil {
				break
			}
			largest, k = p, n
		}
		if largest == nil {
			continue
		}
		c := Admit(Check{By: ByCode, Expr: shape(k), Says: "x"})
		switch {
		case slices.Contains(slow, name) && c.By != ByAI:
			t.Errorf("%s at k=%d is admitted", name, k)
		case c.By == ByCode:
			if why := held(largest); why != "" {
				t.Errorf("%s at k=%d is admitted and %s", name, k, why)
			}
		}
	}
}
