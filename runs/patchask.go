package runs

import (
	"context"
	"crypto/rand"
	"errors"
	"time"
)

// ApprovalLife is how long a production approval stands before it reads expired (contracts/routes.md, Production
// approvals): a person's and a patch's alike.
const ApprovalLife = 24 * time.Hour

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ApprovalCode draws a production approval's code: eight Crockford base32 symbols, stored without the dash a
// person reads (XXXX-XXXX). A caller draws again when it collides with any approval's.
func ApprovalCode() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	code := make([]byte, 8)
	for i, b := range raw {
		code[i] = crockford[b&31] // 32 symbols divide 256: no bias
	}
	return string(code), nil
}

// ShownCode is a code as a person reads it: XXXX-XXXX.
func ShownCode(code string) string { return code[:4] + "-" + code[4:] }

// patchingOn is whether Canyon patches now: this server patches (Patches: CANYON_PATCHING on, sign-in on), it is built
// with Canyon Enterprise (Patcher), and the org's patching reads ask (contracts/routes.md, the preferences row and
// Licence). off stops every patch run before its /run and files no patch approval.
func (p *Pipeline) patchingOn(ctx context.Context, q querier) (bool, error) {
	if !p.Patches.Load() || p.Patcher == nil {
		return false, nil
	}
	var on bool
	err := q.QueryRow(ctx, "SELECT patching = 'ask' FROM org_preferences").Scan(&on)
	return on, err
}

// PatchingOn is whether Canyon patches at all now: the operator's CANYON_PATCHING and the org's patching, before any
// app's own rule (Patching).
func (p *Pipeline) PatchingOn(ctx context.Context, q Querier) (bool, error) {
	return p.patchingOn(ctx, q)
}

// Patching is whether Canyon patches project now, the one condition read live wherever a patch is planned, goes to
// its /run, is asked for production, and its approval read or decided (contracts/routes.md, the preferences row):
// patchingOn, and the app's effective platform.known-vuln on, its additions included. Fail closed: a policy that
// cannot be composed patches nothing.
func (p *Pipeline) Patching(ctx context.Context, q Querier, project string) (bool, error) {
	policyOf := p.ProjectPolicy.Load()
	if on, err := p.patchingOn(ctx, q); err != nil || !on || policyOf == nil {
		return false, err
	}
	snap, err := policyOf(ctx, q, project)
	return err == nil && ReadsImages(snap), err
}

// ErrApproverGone is DecidePatch's answer when the admin who approved the identity for the fleet is no longer an admin:
// nothing is decided in their name (contracts/routes.md, the fleet approval).
var ErrApproverGone = errors.New("the admin who approved the identity is no longer an admin")
