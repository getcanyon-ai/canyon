package runs

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getcanyon-ai/canyon/gate/opengrep"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"go.yaml.in/yaml/v3"
)

// The org's rules repo (contracts/finding.md, Canyon-run tools): fetched in process, over connections
// dialled here, at one commit, capped, and kept by (repo, commit). Variables so a test can lower them.
var (
	rulesCap     = 50 << 20         // what is received, what is unpacked, and the rules part the executor takes
	rulesTimeout = 60 * time.Second // the whole fetch
)

// unfetched is why the org's rules repo could not be fetched, in the contract's words.
type unfetched string

func (u unfetched) Error() string { return "its rules repo could not be fetched: " + string(u) }

const (
	notPublic    unfetched = "its host is not a public address"
	tooSlow      unfetched = "it took over 60 seconds"
	tooBig       unfetched = "it is over 50 MiB"
	noRules      unfetched = "it has no .yml or .yaml file"
	noRulesIn    unfetched = "it has no .yml or .yaml file under its paths"
	notFound     unfetched = "its repository was not found"
	asksForLogin unfetched = "it asks for a credential, and none is set for it"
	refusedLogin unfetched = "it refused the credential set for it"
	redirected   unfetched = "it answered with a redirect, which Canyon never follows"
	badCert      unfetched = "its certificate does not verify"
	noCommit     unfetched = "that commit is not in it"
	notFetched   unfetched = "git could not fetch that commit; the server's log has git's own error"
)

// errRulesRedirect is the fetch's refusal of a redirect, which it never follows.
var errRulesRedirect = errors.New("a redirect")

// notPublicNets are what the fetch never dials: loopback, private, link-local, this network, broadcast,
// multicast, and Teredo, whose IPv4 is hidden (contracts/finding.md).
var notPublicNets = func() (nets []netip.Prefix) {
	for _, p := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fc00::/7",
		"169.254.0.0/16", "fe80::/10", "0.0.0.0/8", "::/128", "255.255.255.255/32", "224.0.0.0/4", "ff00::/8", "2001::/32"} {
		nets = append(nets, netip.MustParsePrefix(p))
	}
	return nets
}()

var nat64, sixToFour = netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("2002::/16")

// public is whether the fetch may dial a: an IPv6 address carrying an IPv4 one is judged as it.
func public(a netip.Addr) bool {
	a, b := a.WithZone("").Unmap(), a.As16()
	switch {
	case nat64.Contains(a):
		a = netip.AddrFrom4([4]byte(b[12:]))
	case sixToFour.Contains(a):
		a = netip.AddrFrom4([4]byte(b[2:6]))
	}
	return !slices.ContainsFunc(notPublicNets, func(p netip.Prefix) bool { return p.Contains(a) })
}

// resolve, dial and roots (nil: the system's) are the fetch's lookup, connection and roots, and createTemp
// the file a fetched commit is written to before it is kept; tests replace them.
var (
	resolve                   = net.DefaultResolver.LookupNetIP
	dial                      = (&net.Dialer{}).DialContext
	roots      *x509.CertPool = nil
	createTemp                = os.CreateTemp
)

// fetch is one fetch: its transport, the address it checked, what was refused and what was received.
type fetch struct {
	transport *http.Transport
	mu        sync.Mutex
	checked   netip.AddrPort
	refused   atomic.Bool
	received  atomic.Int64
}

// dialPublic resolves the repo's host once, refuses it on any address not public, and dials the
// address it checked, then and on every later connection of the fetch: the name is never resolved again.
func (f *fetch) dialPublic(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.checked.IsValid() {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return nil, err
		}
		ips, err := resolve(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 || slices.ContainsFunc(ips, func(a netip.Addr) bool { return !public(a) }) {
			f.refused.Store(true)
			return nil, notPublic
		}
		f.checked = netip.AddrPortFrom(ips[0].WithZone(""), uint16(n))
	}
	return dial(ctx, network, f.checked.String())
}

// RoundTrip counts every body the fetch receives against rulesCap.
func (f *fetch) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := f.transport.RoundTrip(r)
	if err == nil {
		resp.Body = counted{resp.Body, f}
	}
	return resp, err
}

type counted struct {
	io.ReadCloser
	f *fetch
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if c.f.received.Add(int64(n)) > int64(rulesCap) {
		return n, tooBig
	}
	return n, err
}

// orgRules is the repo's rules files at commit, by path: kept, else fetched and kept; a failed fetch is unfetched.
func (p *Pipeline) orgRules(ctx context.Context, repo, commit string, auth transport.AuthMethod) (map[string][]byte, error) {
	sum := sha256.Sum256([]byte(repo))
	kept := filepath.Join(p.Repos, ".opengrep-rules", hex.EncodeToString(sum[:]), commit+".json")
	if raw, err := os.ReadFile(kept); !errors.Is(err, fs.ErrNotExist) { //nolint:gosec // a digest and a 40-hex commit under the server's own directory
		var files map[string][]byte
		return files, cmp.Or(err, json.Unmarshal(raw, &files))
	}
	f := &fetch{}
	f.transport = &http.Transport{Proxy: nil, DialContext: f.dialPublic, ForceAttemptHTTP2: true, // no proxy, whatever the environment says
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}} // SNI and verification take the URL's name, never the address
	defer f.transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, rulesTimeout)
	defer cancel()
	files, err := f.rules(ctx, repo, commit, auth)
	switch {
	case err == nil && len(files) == 0:
		return nil, noRules
	case err == nil:
	case f.refused.Load():
		return nil, notPublic
	case f.received.Load() > int64(rulesCap) || errors.Is(err, tooBig):
		return nil, tooBig
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, tooSlow
	default:
		return nil, unfetchedBy(err, auth != nil)
	}
	raw, err := json.Marshal(files)
	if err := cmp.Or(err, os.MkdirAll(filepath.Dir(kept), 0o700)); err != nil {
		return nil, err
	}
	tmp, err := createTemp(filepath.Dir(kept), "fetch-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(raw)
	if err := cmp.Or(err, tmp.Close()); err != nil { // closed whatever the write did, and never kept after a failed one
		return nil, err
	}
	if err := os.Rename(tmp.Name(), kept); err != nil { // whole or not at all: a reader never sees half a commit
		return nil, err
	}
	return files, nil
}

// unfetchedBy is a failed fetch's reason, read off the error's type: go-git's for the remote's 401, 403 and 404,
// the TLS stack's for a certificate, the fetch's own for a redirect. Anything else is notFetched, and
// git's own error is logged for the operator; a url.Error's URL is dropped first, never the credential
// either, which travels as auth (#1124).
func unfetchedBy(err error, credential bool) unfetched {
	var cert *tls.CertificateVerificationError
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return notFound
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		if credential {
			return refusedLogin
		}
		return asksForLogin
	case errors.Is(err, errRulesRedirect):
		return redirected
	case errors.As(err, &cert):
		return badCert
	case errors.Is(err, plumbing.ErrObjectNotFound):
		return noCommit
	}
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	log.Printf("runs: the org's rules repo could not be fetched: %v", err)
	return notFetched
}

// rules fetches commit from repo at depth 1 over git's smart HTTP, with auth when it is not nil, and
// answers its rules files.
func (f *fetch) rules(ctx context.Context, repo, commit string, auth transport.AuthMethod) (map[string][]byte, error) {
	ep, err := transport.NewEndpoint(repo)
	if err != nil {
		return nil, err
	}
	s, err := githttp.NewClient(&http.Client{Transport: f, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errRulesRedirect // never followed
	}}).NewUploadPackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	adv, err := s.AdvertisedReferencesContext(ctx)
	if err != nil {
		return nil, err
	}
	req := packp.NewUploadPackRequestFromCapabilities(adv.Capabilities)
	for _, c := range []capability.Capability{capability.IncludeTag, capability.Sideband64k, capability.Sideband} {
		req.Capabilities.Delete(c) // no tags, and the pack as sent, not multiplexed with progress
	}
	if err := req.Capabilities.Set(capability.Shallow); err != nil { // depth 1: a server without it refuses the request
		return nil, err
	}
	req.Wants, req.Depth = []plumbing.Hash{plumbing.NewHash(commit)}, packp.DepthCommits(1)
	resp, err := s.UploadPack(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Close() }()
	pack, err := io.ReadAll(resp)
	if err != nil {
		return nil, err
	}
	if err := unpackedWithin(bytes.NewReader(pack)); err != nil {
		return nil, err
	}
	st := memory.NewStorage()
	if err := packfile.UpdateObjectStorage(st, bytes.NewReader(pack)); err != nil {
		return nil, err
	}
	c, err := object.GetCommit(st, plumbing.NewHash(commit)) // the commit asked, by its id: another answered is not found
	if err != nil {
		return nil, err
	}
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	w := object.NewTreeWalker(tree, true, nil)
	defer w.Close()
	for {
		name, e, err := w.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if e.Mode != filemode.Regular && e.Mode != filemode.Executable || !rulesFile(name) {
			continue // a symlink or a submodule is never followed, nor a directory's entry read
		}
		blob, err := object.GetBlob(st, e.Hash)
		if err != nil {
			return nil, err
		}
		r, err := blob.Reader()
		if err != nil {
			return nil, err
		}
		files[name], err = io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			return nil, err
		}
	}
}

// rulesFile is a path whose name ends .yml or .yaml and that has no segment starting with a dot.
func rulesFile(name string) bool {
	ext := path.Ext(name)
	return (ext == ".yml" || ext == ".yaml") && !slices.ContainsFunc(strings.Split(name, "/"), func(s string) bool { return strings.HasPrefix(s, ".") })
}

// objectCost is each object's keep beside its bytes, charged so countless empty objects are over the cap too.
const objectCost = 512

// unpackedWithin is tooBig when the pack's objects, unpacked, are over rulesCap: each object's declared
// size, read before it is inflated, and a delta's the size it builds too, each held to the cap before it
// is summed, so no sum wraps. The scanner never inflates an object past its declared size, and a delta's
// stream, inflated for the size it builds, never writes past what the cap has left.
func unpackedWithin(pack io.Reader) error {
	s := packfile.NewScanner(pack)
	_, objects, err := s.Header()
	if err != nil {
		return err
	}
	total := int64(objects) * objectCost
	for range objects {
		h, err := s.NextObjectHeader()
		if err != nil {
			return err
		}
		size := h.Length
		if size > int64(rulesCap) || total+size > int64(rulesCap) {
			return tooBig
		}
		if h.Type == plumbing.OFSDeltaObject || h.Type == plumbing.REFDeltaObject {
			head := headOf{left: int64(rulesCap) - total}
			if _, _, err := s.NextObject(&head); err != nil {
				return err
			}
			_, n := binary.Uvarint(head.b) // a delta starts with the size of its base, then the size it builds: git's varints are Go's
			built, m := binary.Uvarint(head.b[max(n, 0):])
			if n <= 0 || m <= 0 {
				return errors.New("a delta without its sizes")
			}
			if built > uint64(rulesCap) { //nolint:gosec // rulesCap is a positive size
				return tooBig
			}
			size = max(size, int64(built)) //nolint:gosec // at most rulesCap, just checked
			if total+size > int64(rulesCap) {
				return tooBig
			}
		}
		total += size
	}
	return nil
}

// headOf keeps the first 20 bytes written to it, a delta's two sizes, and takes no more than left.
type headOf struct {
	b    []byte
	left int64
}

func (h *headOf) Write(p []byte) (int, error) {
	if h.left -= int64(len(p)); h.left < 0 {
		return 0, tooBig
	}
	h.b = append(h.b, p[:min(len(p), 20-len(h.b))]...)
	return len(p), nil
}

// rulesTar is opengrep's rules part: the vendored sast-rules at their paths in GitLab's
// repository, and the org's under org/ when set; over the executor's cap it is tooBig
// (contracts/finding.md, Canyon-run tools).
func rulesTar(org map[string][]byte) ([]byte, error) {
	vendored, err := opengrep.Rules()
	if err != nil {
		return nil, err
	}
	bundled := ruleIDs(vendored)
	for _, name := range slices.Sorted(maps.Keys(org)) {
		for _, id := range slices.Sorted(maps.Keys(ruleIDs(map[string][]byte{name: org[name]}))) {
			if bundled[id] {
				return nil, unfetched("its rule " + id + " has the id of one of Canyon's rules")
			}
		}
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := writeRules(tw, "", vendored); err != nil {
		return nil, err
	}
	if err := cmp.Or(writeRules(tw, "org/", org), tw.Close()); err != nil {
		return nil, err
	}
	if b.Len() > rulesCap {
		return nil, tooBig
	}
	return b.Bytes(), nil
}

// ruleIDs is the rule ids the files declare. Opengrep runs with --no-rewrite-rule-ids and its SARIF
// keeps one rule entry per id, so an org rule with a bundled rule's id would take that rule's tags
// (contracts/finding.md, Canyon-run tools). A file that is not YAML declares none here: opengrep names
// it, as a rule that could not load.
func ruleIDs(files map[string][]byte) map[string]bool {
	ids := map[string]bool{}
	for _, b := range files {
		var f struct {
			Rules []struct {
				ID string `yaml:"id"`
			} `yaml:"rules"`
		}
		if yaml.Unmarshal(b, &f) == nil {
			for _, r := range f.Rules {
				ids[r.ID] = true
			}
		}
	}
	return ids
}

// under is the files whose path is one of paths or begins with one and /; none is noRulesIn.
func under(files map[string][]byte, paths []string) (map[string][]byte, error) {
	kept := map[string][]byte{}
	for name, b := range files {
		if slices.ContainsFunc(paths, func(p string) bool { return name == p || strings.HasPrefix(name, p+"/") }) {
			kept[name] = b
		}
	}
	if len(kept) == 0 {
		return nil, noRulesIn
	}
	return kept, nil
}

// writeRules writes files into tw under prefix, in name order.
func writeRules(tw *tar.Writer, prefix string, files map[string][]byte) error {
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: prefix + name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return err
		}
	}
	return nil
}
