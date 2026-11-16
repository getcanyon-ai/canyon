package identity

import (
	"maps"
	"slices"
)

// MaxGroups is how many groups a sign-in stores: Entra stops sending the claim past about 200, and a
// list longer than that is read as none rather than cut, so no grant ever matches half a list.
const MaxGroups = 200

// Groups is a groups claim's names as the server and the sidecar both read it (contracts/routes.md, App
// teams): an array's strings, or an object's keys in name order (Zitadel's project roles), at most
// MaxGroups, else none, with the word for the log — ok, absent (Entra sends none past its overage
// limit), not-a-list, or over-200.
func Groups(raw any, present bool) ([]string, string) {
	list, isList := raw.([]any)
	if roles, isObject := raw.(map[string]any); isObject {
		list, isList = nil, true
		for _, k := range slices.Sorted(maps.Keys(roles)) {
			list = append(list, k)
		}
	}
	switch {
	case !present:
		return []string{}, "absent"
	case !isList:
		return []string{}, "not-a-list"
	case len(list) > MaxGroups:
		return []string{}, "over-200"
	}
	groups := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return []string{}, "not-a-list"
		}
		groups = append(groups, s)
	}
	return groups, "ok"
}
