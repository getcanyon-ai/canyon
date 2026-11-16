package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// declaredName is a declared resource's or role's name (contracts/routes.md, POST /projects/:id/packages).
var declaredName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// declarationError refuses an upload for its canyon.toml: 400 with this error code and message.
type declarationError struct{ code, msg string }

func (e *declarationError) Error() string { return e.msg }

func refuseDeclaration(code, format string, args ...any) error {
	return &declarationError{code, fmt.Sprintf(format, args...)}
}

// maxCanyonTOML bounds what the server reads of an upload's canyon.toml (contracts/routes.md).
const maxCanyonTOML = 64 << 10

// declarations reads what the canyon.toml at the root of an unpacked package declares, in one pass
// under one cap: [resources] checked against the component menu (name to type), the top-level
// description, and [roles] (name to what the role lets a person do). No canyon.toml, or none of a
// section, declares nothing. Only the server reads past the header; the CLI stops at it (cli/project.go).
func declarations(tree string, menu []componentType) (resources, roles map[string]string, description *string, err error) {
	f, err := os.Open(filepath.Join(tree, "canyon.toml")) //nolint:gosec // the upload's own tree, unpacked into our temp dir
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, nil
	}
	var body []byte
	if err == nil {
		defer func() { _ = f.Close() }()
		body, err = io.ReadAll(io.LimitReader(f, maxCanyonTOML+1))
	}
	if err != nil {
		return nil, nil, nil, refuseDeclaration("invalid_resources", "canyon.toml at the package root is not a file Canyon can read")
	}
	if len(body) > maxCanyonTOML {
		return nil, nil, nil, refuseDeclaration("invalid_resources", "canyon.toml is larger than 64 KiB")
	}
	var file map[string]any
	if err := toml.Unmarshal(body, &file); err != nil {
		return nil, nil, nil, refuseDeclaration("invalid_resources", "canyon.toml is not TOML: %v", err)
	}
	if resources, err = declaredResources(file, body, menu); err != nil {
		return nil, nil, nil, err
	}
	if description, err = declaredDescription(file); err != nil {
		return nil, nil, nil, err
	}
	if roles, err = declaredRoles(file, body); err != nil {
		return nil, nil, nil, err
	}
	return resources, roles, description, nil
}

// declaredResources is [resources], name to type: nil when the file has none.
func declaredResources(file map[string]any, body []byte, menu []componentType) (map[string]string, error) {
	raw, ok := file["resources"]
	if !ok {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok || !isASection(body, "resources") {
		return nil, refuseDeclaration("invalid_resources", "resources in canyon.toml is a [resources] section of name = { type = \"<type>\" } lines")
	}
	types := make([]string, 0, len(menu))
	for _, row := range menu {
		types = append(types, row.Type)
	}
	// givenBy maps an env var to the resource giving it; names go in order, so a refusal names the same two every time
	declared, givenBy := map[string]string{}, map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(table)) {
		if !declaredName.MatchString(name) {
			return nil, refuseDeclaration("invalid_resources", "resource %q: a resource name is lowercase letters, digits and dashes, starting with a letter, at most 40 characters", name)
		}
		entry, _ := table[name].(map[string]any)
		typ, isString := entry["type"].(string)
		if len(entry) != 1 || !isString {
			return nil, refuseDeclaration("invalid_resources", "resource %q: a declaration is %s = { type = \"<type>\" } and nothing else", name, name)
		}
		i := slices.Index(types, typ)
		if i < 0 {
			return nil, refuseDeclaration("invalid_resources", "resource %q has type %q, which this host does not offer: the types are %s", name, typ, strings.Join(types, ", "))
		}
		if menu[i].Status != "available" { // a deploy would fail at its first resource: said here, where it can be fixed (#1255)
			return nil, refuseDeclaration("invalid_resources", "resource %q has type %q, which this host does not provision yet: ask whoever runs it to set that up, or remove the declaration", name, typ)
		}
		for _, env := range menu[i].Gives {
			if other, taken := givenBy[env]; taken {
				return nil, refuseDeclaration("invalid_resources", "resources %q and %q both give the app %s, and an app gets one of each: declare one of them", other, name, env)
			}
			givenBy[env] = name
		}
		declared[name] = typ
	}
	return declared, nil
}

// declaredDescription is the top-level description, one line saying what the app does: nil when the file has none.
func declaredDescription(file map[string]any) (*string, error) {
	raw, ok := file["description"]
	if !ok {
		return nil, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, refuseDeclaration("invalid_description", `description in canyon.toml is one line of text in quotes: description = "what the app does"`)
	}
	if n := utf8.RuneCountInString(text); n > 200 {
		return nil, refuseDeclaration("invalid_description", "description in canyon.toml is %d characters: at most 200", n)
	}
	if strings.ContainsFunc(text, hidesText) { // every signed-in member reads it, beside other teams' apps
		return nil, refuseDeclaration("invalid_description", "description in canyon.toml is one line of plain text: no line breaks or control characters")
	}
	return &text, nil
}

// declaredRoles is [roles], the app roles an owner may grant, name to what the role lets a person do:
// nil when the file has none.
func declaredRoles(file map[string]any, body []byte) (map[string]string, error) {
	raw, ok := file["roles"]
	if !ok {
		return nil, nil
	}
	table, ok := raw.(map[string]any)
	if !ok || !isASection(body, "roles") {
		return nil, refuseDeclaration("invalid_roles", `roles in canyon.toml is a [roles] section of name = "what the role lets a person do" lines`)
	}
	if len(table) > 16 {
		return nil, refuseDeclaration("invalid_roles", "canyon.toml declares %d roles: an app has at most 16", len(table))
	}
	roles := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(table)) { // in order, so a refusal names the same role every time
		if !declaredName.MatchString(name) {
			return nil, refuseDeclaration("invalid_roles", "role %q: a role name is lowercase letters, digits and dashes, starting with a letter, at most 40 characters, as in expense-approver", name)
		}
		line, isString := table[name].(string)
		if !isString {
			return nil, refuseDeclaration("invalid_roles", `role %q: a role is %s = "what it lets a person do" and nothing else`, name, name)
		}
		if n := utf8.RuneCountInString(line); n > 120 {
			return nil, refuseDeclaration("invalid_roles", "role %q: its line is %d characters, at most 120", name, n)
		}
		if strings.ContainsFunc(line, hidesText) {
			return nil, refuseDeclaration("invalid_roles", "role %q: its line is one line of plain text: no line breaks or control characters", name)
		}
		roles[name] = line
	}
	return roles, nil
}

// isASection is whether section is a [section] header of one-part keys — never a [section.x]
// table, a dotted key, or section as a key's value.
func isASection(body []byte, section string) bool {
	p := unstable.Parser{}
	p.Reset(body)
	var table []string // the header the expression sits under: nil at the top
	for p.NextExpression() {
		e := p.Expression()
		var key []string
		for it := e.Key(); it.Next(); {
			key = append(key, string(it.Node().Data))
		}
		switch e.Kind {
		case unstable.Table, unstable.ArrayTable:
			if key[0] == section && (e.Kind == unstable.ArrayTable || len(key) > 1) {
				return false
			}
			table = key
		case unstable.KeyValue:
			if table == nil && key[0] == section || slices.Equal(table, []string{section}) && len(key) > 1 {
				return false
			}
		}
	}
	return p.Error() == nil
}

// firstDeclarations is the declared names no package of the project declared with that type before, a
// pull request's or a check's aside (their declarations are not the project's): one audit row each.
func firstDeclarations(ctx context.Context, q querier, project string, declared map[string]string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT d.key FROM jsonb_each_text($2::jsonb) d
		WHERE NOT EXISTS (SELECT 1 FROM packages WHERE project_id = $1 AND source_kind NOT IN ('repo-pr', 'check') AND resources @> jsonb_build_object(d.key, d.value)) ORDER BY 1`, project, declared)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// resourceRow is one element of resources[] on the project read (contracts/routes.md): a record,
// with its env, one the org's platform provided, with its env and endpoint, or a declaration of the
// current package neither answers yet, state declared.
type resourceRow struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	Env      *string `json:"env,omitempty"`
	State    string  `json:"state"`
	Endpoint *string `json:"endpoint,omitempty"` // provided's alone (#2432)
	Driver   string  `json:"-"`                  // the name of the driver that made it: a record's, "" on a declaration
}

// providedSQL is where the org's platform says the project's resources are (#2432), as rows of resources[]: each
// typed by the reporting release's package. A record of Canyon's of the same env and name stays beside it: the
// switch moved no data, and a delete destroys both.
const providedSQL = `SELECT pr.name, pk.resources->>pr.name, pr.env, 'provided', pr.endpoint
FROM platform_resources pr JOIN releases rl ON rl.id = pr.release_id JOIN packages pk ON pk.id = rl.package_id AND pk.project_id = rl.project_id
WHERE pr.project_id = $1 AND pk.resources ? pr.name`

// resourcesSQL is the project's records, what the platform provided, and its current package's declarations
// neither names; by name, the declaration first, then preview before prod, a record before a provided row.
const resourcesSQL = `WITH head AS (
	SELECT resources FROM packages WHERE project_id = $1 AND source_kind NOT IN ('repo-pr', 'check') ORDER BY created_at DESC, id DESC LIMIT 1
)
SELECT * FROM (
SELECT name, type, env, state, NULL::text AS endpoint FROM resources WHERE project_id = $1
UNION ALL
` + providedSQL + `
UNION ALL
SELECT d.key, d.value, NULL, 'declared', NULL FROM head, jsonb_each_text(COALESCE(head.resources, '{}')) d
WHERE NOT EXISTS (SELECT 1 FROM resources r WHERE r.project_id = $1 AND r.name = d.key)
	AND NOT EXISTS (SELECT 1 FROM platform_resources pr WHERE pr.project_id = $1 AND pr.name = d.key)
) r ORDER BY name, env NULLS FIRST, state = 'provided'`

func resourcesOf(ctx context.Context, q querier, project string) ([]resourceRow, error) {
	rows, err := q.Query(ctx, resourcesSQL, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []resourceRow{}
	for rows.Next() {
		var r resourceRow
		if err := rows.Scan(&r.Name, &r.Type, &r.Env, &r.State, &r.Endpoint); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
