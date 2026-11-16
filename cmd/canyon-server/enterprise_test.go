package main

import "github.com/getcanyon-ai/canyon/vault"

// testEE is the next test server's Canyon Enterprise: none here, a build without it; licence_test.go, in a build with
// it, sets the licensed hooks the tests of what an Enterprise feature does run with.
var testEE = func(*vault.Vault) eeHooks { return eeHooks{} }

// The 409 licence_required bodies, by feature (contracts/routes.md, Licence; design/EXPERIENCE.md, D5): a server with no
// licence and one built without Canyon Enterprise answer each change the same.
const (
	outboundKeysRequired = `{"error":"licence_required","feature":"outbound_keys","message":"Adding credentials to apps' calls needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence."}`
	patchingRequired     = `{"error":"licence_required","feature":"platform_patching","message":"Turning on patching needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence."}`
	groupsRequired       = `{"error":"licence_required","feature":"group_sync","message":"Access by sign-in group needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence."}`
)
