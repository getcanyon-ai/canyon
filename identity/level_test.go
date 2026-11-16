package identity

import (
	"strings"
	"testing"
)

// A person's level is the highest grant naming them — by address, by subject once they have signed in,
// by group, or everyone — and their roles the union of those grants'; a grant naming someone else
// counts for nothing, and no grant is none with [] roles.
func TestLevelIsTheHighestGrantNamingThePerson(t *testing.T) {
	grants := []Grant{{Kind: "everyone", Level: "viewer", Roles: []string{"reader"}}, {Kind: "group", Grantee: "claims-team", Level: "contributor", Roles: []string{"approver"}},
		{Kind: "email", Grantee: "dana@meridian.example", Subject: "u-dana", Level: "owner", Roles: []string{"approver"}},
		{Kind: "email", Grantee: "priya@meridian.example", Level: "owner", Roles: []string{"admin"}}}
	for _, c := range [][5]string{ // email, subject, group, level, roles
		{"dana.w@meridian.example", "u-dana", "", "owner", "approver reader"}, // the address changed, the subject did not
		{"dana@meridian.example", "u-someone", "", "viewer", "reader"},        // the address another person's now: a bound grant is its subject's alone
		{"dana@meridian.example", "", "", "viewer", "reader"},                 // no subject to compare: the bound grant names nobody (#2220)
		{"priya@meridian.example", "", "", "owner", "admin reader"},           // bound to none: by the address
		{"priya@meridian.example", "u-priya", "", "owner", "admin reader"},
		{"sam@meridian.example", "u-sam", "claims-team", "contributor", "approver reader"},
		{"sam@meridian.example", "", "", "viewer", "reader"}, // an empty subject names nobody
	} {
		if level, roles := Level(c[0], c[1], []string{c[2]}, grants); level != c[3] || strings.Join(roles, " ") != c[4] {
			t.Errorf("%v: %s %v", c, level, roles)
		}
	}
	if level, roles := Level("sam@meridian.example", "u-sam", nil, grants[1:]); level != "none" || roles == nil || len(roles) != 0 {
		t.Fatalf("no grant naming them: %s %v", level, roles)
	}
}
