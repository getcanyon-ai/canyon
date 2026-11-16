// Package names holds the id patterns the server and the CLI both check against, so one pattern
// answers both sides, and the words the host makes a new app's id of, for every server path that makes
// one. Stdlib-only: the CLI links nothing that talks to a database.
package names

import "regexp"

// Project is the seam's project id pattern (contracts/schema/timeline-row.schema.json): a DNS
// label, the first label of the app's hostname (contracts/routes.md).
var Project = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ValidProject reports whether s is a project id.
func ValidProject(s string) bool { return Project.MatchString(s) }
