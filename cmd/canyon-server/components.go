package main

import (
	"fmt"

	"github.com/getcanyon-ai/canyon/provisioners"
)

// componentType is one row of GET /component-types (contracts/routes.md): the prose shape of a
// component_menu entry. The manifest hands out the bare Score names; this shape carries what the
// type provisions and whether this host can provision it today.
type componentType struct {
	Type   string   `json:"type"`   // the Score resource type name (score.dev)
	Class  string   `json:"class"`  // Score's resource class, never a Canyon category
	Gives  []string `json:"gives"`  // the env var names the type provisions when declared
	Status string   `json:"status"` // available | planned: whether this host can provision the type today
	Notes  string   `json:"notes"`  // agent-read product copy
}

// componentTable describes exactly the names the snapshot's menu carries, both planned until
// provisioned says otherwise. M4's registry table replaces this and deletes it, and with
// it the boot-time check in loadSnapshot: that runs over embedded bytes, and a table in the
// database can only be checked once the pool is up, so the check moves to a startup step then.
var componentTable = []componentType{
	{Type: "postgres", Class: "default", Status: "planned", Notes: "An isolated database and role per app. Declare it and deploy; do not install a database or write a connection string."},
	{Type: "s3", Class: "default", Status: "planned", Notes: "An S3-compatible bucket and a scoped credential per app. Declare it and deploy; do not create a bucket or write a credential."},
}

// componentsOf is one row per menu entry, in the menu's order; a name the table cannot describe
// is an error naming it, so loadSnapshot refuses the boot rather than the route shipping a hole.
func componentsOf(menu []string) ([]componentType, error) {
	rows := make([]componentType, 0, len(menu))
	seen := make(map[string]bool, len(menu))
	for _, name := range menu {
		if seen[name] { // a menu is a set: naming a type twice is a snapshot defect, not two rows
			return nil, fmt.Errorf("component_menu names %q twice", name)
		}
		seen[name] = true
		var found bool
		for _, row := range componentTable {
			if row.Type == name {
				row.Gives = provisioners.Promised(name) // the driver's promise is the one list of names
				rows = append(rows, row)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("component_menu names %q, which this server cannot describe", name)
		}
	}
	return rows, nil
}
