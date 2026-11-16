package identity

import "slices"

// Grant is one entry of an app's access (contracts/executor.md, PUT /access: entries[]): a person by
// address, an identity-provider group, or everyone, at a level, with the app roles it carries.
type Grant struct {
	Kind    string   `json:"kind"`    // email | group | everyone
	Grantee string   `json:"grantee"` // the lower-cased address, the group, "" for everyone
	Subject string   `json:"subject"` // the person's identity-provider sub, "" until they first sign in
	Level   string   `json:"level"`   // owner | contributor | viewer
	Roles   []string `json:"roles"`
}

var rank = map[string]int{"viewer": 1, "contributor": 2, "owner": 3}

// Level is a member's level on an app and their app roles (design/PERMISSIONS.md): the highest level
// of the grants naming them — a person's bound to a subject by that subject alone ("" names none), one
// bound to none by their address — or one of their groups, or everyone, and the union of those grants'
// roles, sorted; none and [] when no grant names them. The caller checks membership first (everyone is
// the org's members) and passes an admin itself.
func Level(email, subject string, groups []string, grants []Grant) (string, []string) {
	level, roles := "none", []string{}
	for _, g := range grants {
		if g.Kind == "email" && (g.Subject == "" && g.Grantee == email || g.Subject != "" && g.Subject == subject) ||
			g.Kind == "group" && slices.Contains(groups, g.Grantee) || g.Kind == "everyone" {
			if rank[g.Level] > rank[level] {
				level = g.Level
			}
			roles = append(roles, g.Roles...)
		}
	}
	slices.Sort(roles)
	return level, slices.Compact(roles)
}
