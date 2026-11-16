package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/package-url/packageurl-go"
)

// An sbom target's part is a slice of the SBOM trivy itself wrote for an image (contracts/executor.md,
// target sbom), and is refused unless it has exactly that shape: trivy's parser skips, renames or nests
// what it cannot place without a word, so the executor never predicts it — it holds the part to what
// trivy 0.74.0 writes, and after the run to what trivy says it read (considered).

// checkPart is a part trivy reads whole, as the executor forwards it: b parsed once, within its bounds as
// it streams, and written again (canonical), then that one CycloneDX JSON document held to the shape of
// trivy's own (whole). What is validated and what trivy reads are the same bytes.
func checkPart(b []byte) ([]byte, error) {
	c, err := canonical(b)
	if err != nil {
		return nil, err
	}
	if _, ok := sbomOf(c); !ok {
		return nil, errors.New("not one CycloneDX JSON document")
	}
	return c, whole(c)
}

// The part's bounds, held as it streams, before anything is built past them. The part itself is at most
// maxScanOut, which the body cap holds before any of this; past it, the decoder never holds more than
// maxString of one token unread (perToken), and canonical writes at most twice the part, strings written
// without HTML escaping.
const (
	maxBOMRef = 2 << 10   // a bom-ref: its form is trivy's, its length the executor's
	maxTokens = 1_000_000 // a 10 MiB SBOM of trivy's holds about one token in ten bytes
	maxDepth  = 16        // trivy's own SBOM nests 7 deep
	maxArray  = 50_000    // any array: components, properties, licenses, hashes, dependencies
	maxString = 64 << 10  // any key or string
)

// canonical is b parsed once, token by token, and written again as it streams: one JSON document of
// valid UTF-8 with no byte-order mark, at most maxTokens tokens nested at most maxDepth deep, no array
// longer than maxArray, no key or string longer than maxString or holding U+FFFD (what a lone surrogate
// decodes to), and no object with a key twice — Go keeps the last of two, another reader the first.
func canonical(b []byte) ([]byte, error) {
	switch {
	case bytes.HasPrefix(b, []byte("\xef\xbb\xbf")):
		return nil, errors.New("it begins with a byte-order mark")
	case !utf8.Valid(b):
		return nil, errors.New("it is not valid UTF-8")
	}
	src := &perToken{r: bytes.NewReader(b)}
	dec := json.NewDecoder(src)
	src.held = func() int64 { return src.read - dec.InputOffset() }
	dec.UseNumber()
	p := &parser{dec: dec}
	var out bytes.Buffer
	if err := p.value(&out, 0); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("more follows its one JSON document")
	}
	return out.Bytes(), nil
}

// perToken is the part as canonical's decoder reads it, refusing to read on while the decoder holds more
// than maxString of it unread: one token — a key, a string, a number — is cut off at its bound while it is
// read, never materialised past it.
type perToken struct {
	r    io.Reader
	read int64
	held func() int64
}

func (p *perToken) Read(b []byte) (int, error) {
	if p.held() > maxString {
		return 0, fmt.Errorf("a token of more than %d bytes", maxString)
	}
	n, err := p.r.Read(b[:min(len(b), maxString)])
	p.read += int64(n)
	return n, err
}

// parser is canonical's one pass: each token read is counted, and written as it is checked.
type parser struct {
	dec    *json.Decoder
	tokens int
}

func (p *parser) token() (json.Token, error) {
	if p.tokens++; p.tokens > maxTokens {
		return nil, fmt.Errorf("more than %d tokens", maxTokens)
	}
	return p.dec.Token()
}

func (p *parser) value(w *bytes.Buffer, depth int) error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	switch t := tok.(type) {
	case json.Delim:
		if depth == maxDepth {
			return fmt.Errorf("nested more than %d deep", maxDepth)
		}
		object := t == '{'
		w.WriteByte(map[bool]byte{true: '{', false: '['}[object])
		seen := map[string]bool{}
		for n := 0; p.dec.More(); n++ {
			if n > 0 {
				w.WriteByte(',')
			}
			if !object {
				if n == maxArray {
					return fmt.Errorf("an array of more than %d values", maxArray)
				}
			} else {
				k, err := p.token()
				if err != nil {
					return err
				}
				key, _ := k.(string) // an object's key is a string: the decoder refuses any other
				if err := checkString(key); err != nil {
					return err
				}
				if seen[key] {
					return fmt.Errorf("an object has the key %q twice", key)
				}
				seen[key] = true
				writeString(w, key)
				w.WriteByte(':')
			}
			if err := p.value(w, depth+1); err != nil {
				return err
			}
		}
		if _, err := p.token(); err != nil { // its close
			return err
		}
		w.WriteByte(map[bool]byte{true: '}', false: ']'}[object])
	case string:
		if err := checkString(t); err != nil {
			return err
		}
		writeString(w, t)
	case json.Number:
		w.WriteString(t.String())
	case bool:
		w.WriteString(strconv.FormatBool(t))
	case nil:
		w.WriteString("null")
	}
	return nil
}

func checkString(s string) error {
	switch {
	case len(s) > maxString:
		return fmt.Errorf("a string of more than %d bytes", maxString)
	case strings.ContainsRune(s, utf8.RuneError):
		return errors.New("a string holds U+FFFD, or a lone surrogate that decodes to it")
	}
	return nil
}

// writeString writes s as JSON, without HTML escaping: no longer than twice what it was read from.
func writeString(w *bytes.Buffer, s string) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)       // a string of valid UTF-8 into memory: nothing to fail on
	w.Truncate(w.Len() - 1) // the newline Encode ends with
}

// component is a CycloneDX component as whole and considered read it; Components is only whether it
// nests any.
type component struct {
	BOMRef     string          `json:"bom-ref"`
	Type       string          `json:"type"`
	Group      string          `json:"group"`
	Name       string          `json:"name"`
	Version    string          `json:"version"`
	Purl       string          `json:"purl"`
	Components json.RawMessage `json:"components"`
}

// matchedTypes are the component types trivy 0.74.0 writes in an image's SBOM, below its own container.
var matchedTypes = []string{"library", "application", "operating-system"}

// whole is nil when b has the shape of a slice of trivy's own image SBOM (contracts/executor.md, target
// sbom): components at the top level alone, metadata.component trivy's container with none nested and no
// purl, each component of a type trivy writes and nesting none, each bom-ref its component's own — its
// purl, or a UUID where trivy's purls repeat — and unique in the document, a purl on a library component
// alone, every purl one packageurl-go parses whose component's name, group and version are what trivy
// writes for it (trivyShape), and a part holding operating-system packages has exactly one
// operating-system component.
func whole(b []byte) error {
	var doc struct {
		Metadata struct {
			Component *component
		}
		Components []component
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	refs := map[string]bool{}
	if m := doc.Metadata.Component; m != nil {
		switch {
		case m.Type != "container" || m.Purl != "" || m.Components != nil:
			return errors.New("metadata.component is not trivy's container: of type container, with no purl and no components")
		case m.BOMRef != "":
			refs[m.BOMRef] = true
		}
	}
	var systems, packages int
	for _, c := range doc.Components {
		ref := strconv.Quote(c.BOMRef)
		switch {
		case !slices.Contains(matchedTypes, c.Type):
			return fmt.Errorf("component %s is of type %q, not one trivy writes (library, application, operating-system)", ref, c.Type)
		case c.Components != nil:
			return fmt.Errorf("component %s nests components, which trivy does not read", ref)
		case c.BOMRef == "":
			return errors.New("a component has no bom-ref")
		case refs[c.BOMRef]:
			return fmt.Errorf("bom-ref %s names more than one component", ref)
		case len(c.BOMRef) > maxBOMRef:
			return fmt.Errorf("a bom-ref of more than %d bytes", maxBOMRef)
		case c.Purl != "" && c.Type != "library":
			return fmt.Errorf("component %s carries a purl, which trivy writes on a library alone", ref)
		}
		refs[c.BOMRef] = true
		if c.Type == "operating-system" {
			systems++
		}
		if c.Purl == "" {
			continue
		}
		p, err := packageurl.FromString(c.Purl)
		if err != nil {
			return fmt.Errorf("component %s: %v", ref, err)
		}
		if err := trivyShape(c, p); err != nil {
			return fmt.Errorf("component %s: %v", ref, err)
		}
		if osPurl(p) {
			packages++
		}
	}
	if packages > 0 && systems != 1 {
		return fmt.Errorf("%d operating-system packages beside %d operating-system components, not one", packages, systems)
	}
	return nil
}

// trivyShape is nil when a library component's group, name and version are what trivy 0.74.0 writes
// beside its purl (pkg/sbom/io/encode.go, component; pkg/purl/purl.go, New), so the name trivy matches by
// — its decoder takes the component's name, not the purl's — is the purl's: for npm and maven the purl's
// namespace and name as group and name; for any other type no group, and a name from which trivy derives
// the purl's namespace, name and subpath; and the purl's version, with "<epoch>:" before it when its epoch
// qualifier is not 0.
func trivyShape(c component, p packageurl.PackageURL) error {
	version := p.Version
	if epoch := p.Qualifiers.Map()["epoch"]; epoch != "" && epoch != "0" {
		version = epoch + ":" + version
	}
	if c.Version != version {
		return fmt.Errorf("its version %q is not its purl's %q", c.Version, version)
	}
	if p.Type == packageurl.TypeNPM || p.Type == packageurl.TypeMaven {
		if c.Group != p.Namespace || c.Name != p.Name {
			return fmt.Errorf("its group and name %q %q are not its purl's %q %q", c.Group, c.Name, p.Namespace, p.Name)
		}
		return nil
	}
	if c.Group != "" {
		return fmt.Errorf("it has group %q, which trivy writes for npm and maven alone", c.Group)
	}
	namespace, name, subpath := "", c.Name, ""
	switch p.Type {
	case packageurl.TypeDebian, packageurl.TypeRPM, packageurl.TypeApk: // the namespace is the distribution's
		namespace = p.Namespace
	case packageurl.TypeGolang, packageurl.TypeComposer, packageurl.TypeSwift, packageurl.TypeJulia:
		if i := strings.LastIndex(name, "/"); i >= 0 {
			namespace, name = name[:i], name[i+1:]
		}
	case packageurl.TypeCocoapods:
		name, subpath, _ = strings.Cut(name, "/")
	}
	derived := packageurl.PackageURL{Type: p.Type, Namespace: namespace, Name: name, Version: p.Version, Qualifiers: p.Qualifiers, Subpath: subpath}
	if err := derived.Normalize(); err != nil || derived.Namespace != p.Namespace || derived.Name != p.Name || derived.Subpath != p.Subpath {
		return fmt.Errorf("its name %q is not one trivy writes for its purl %q", c.Name, p.String())
	}
	return nil
}

// The purls trivy 0.74.0 scans, and none other: a package it lists but has no driver for, or whose driver
// matches nothing, is never matched (contracts/executor.md, Listed is not scanned). Each entry cites the
// trivy 0.74.0 source that makes it scanned — a driver with a real Detect — so a trivy upgrade re-checks
// each; pinned here alone, with trivyImage.
var (
	// scannedTypes are the purl types whose language type (pkg/purl/purl.go, LangType) has a driver in
	// pkg/detector/library/driver.go, NewDriver; every one matches through one real Detect,
	// Driver.DetectVulnerabilities (driver.go:115). Conda's has none (driver.go:75), and julia and k8s purls
	// reach none (LangType knows no julia; k8s's drivers are upstream's alone).
	scannedTypes = []string{
		packageurl.TypeGem,       // scanned (has a real Detect): driver.go:30
		packageurl.TypeCargo,     // scanned (has a real Detect): driver.go:33
		packageurl.TypeComposer,  // scanned (has a real Detect): driver.go:36
		packageurl.TypeGolang,    // scanned (has a real Detect): driver.go:39
		packageurl.TypeMaven,     // scanned (has a real Detect): driver.go:42
		packageurl.TypeNPM,       // scanned (has a real Detect): driver.go:45
		packageurl.TypeNuget,     // scanned (has a real Detect): driver.go:48
		packageurl.TypePyPi,      // scanned (has a real Detect): driver.go:51
		packageurl.TypePub,       // scanned (has a real Detect): driver.go:54
		packageurl.TypeHex,       // scanned (has a real Detect): driver.go:57
		packageurl.TypeConan,     // scanned (has a real Detect): driver.go:60
		packageurl.TypeSwift,     // scanned (has a real Detect): driver.go:65
		packageurl.TypeCocoapods, // scanned (has a real Detect): driver.go:70
		packageurl.TypeBitnami,   // scanned (has a real Detect): driver.go:78
	}
	// osDrivers are the distributions whose operating-system driver (pkg/detector/ospkg/detect.go, drivers)
	// has a real Detect, which their deb, rpm and apk packages are scanned by beside the part's
	// operating-system component naming one. CoreOS's driver is registered (detect.go:69) but its Detect
	// returns nothing for every package (pkg/detector/ospkg/coreos/coreos.go:21-23): not scanned.
	osDrivers = []string{
		"alpine",              // scanned (has a real Detect): ospkg/alpine/alpine.go:70
		"alma",                // scanned (has a real Detect): ospkg/alma/alma.go:43
		"amazon",              // scanned (has a real Detect): ospkg/amazon/amazon.go:44
		"azurelinux",          // scanned (has a real Detect): ospkg/azure/azure.go:39
		"cbl-mariner",         // scanned (has a real Detect): ospkg/azure/azure.go:39
		"bottlerocket",        // scanned (has a real Detect): ospkg/bottlerocket/bottlerocket.go:29
		"chainguard",          // scanned (has a real Detect): ospkg/chainguard/chainguard.go:31
		"debian",              // scanned (has a real Detect): ospkg/debian/debian.go:58
		"echo",                // scanned (has a real Detect): ospkg/echo/echo.go:29
		"minimos",             // scanned (has a real Detect): ospkg/minimos/minimos.go:31
		"oracle",              // scanned (has a real Detect): ospkg/oracle/oracle.go:47
		"photon",              // scanned (has a real Detect): ospkg/photon/photon.go:44
		"redhat",              // scanned (has a real Detect): ospkg/redhat/redhat.go:77
		"centos",              // scanned (has a real Detect): ospkg/redhat/redhat.go:77
		"rocky",               // scanned (has a real Detect): ospkg/rocky/rocky.go:41
		"opensuse-tumbleweed", // scanned (has a real Detect): ospkg/suse/suse.go:121
		"opensuse-leap",       // scanned (has a real Detect): ospkg/suse/suse.go:121
		"sles",                // scanned (has a real Detect): ospkg/suse/suse.go:121
		"slem",                // scanned (has a real Detect): ospkg/suse/suse.go:121
		"ubuntu",              // scanned (has a real Detect): ospkg/ubuntu/ubuntu.go:104
		"wolfi",               // scanned (has a real Detect): ospkg/wolfi/wolfi.go:31
	}
)

// trivySkips are the packages trivy 0.74.0's detectors skip although their type is scanned, by version,
// name or qualifier, each line naming its source; system is the part's operating-system component's name.
// Its drop of third-party-repository packages (pkg/detector/ospkg/driver/driver.go) reads a field no SBOM
// carries, so it skips nothing here.
var trivySkips = []func(p packageurl.PackageURL, system string) bool{
	// pkg/detector/library/detect.go (detect), and the ospkg drivers that skip an empty version (amazon.go, bottlerocket.go)
	func(p packageurl.PackageURL, _ string) bool { return p.Version == "" },
	// pkg/detector/ospkg/detect.go (Detect): gpg-pubkey carries no real version, for every driver
	func(p packageurl.PackageURL, _ string) bool { return osPurl(p) && p.Name == "gpg-pubkey" },
	// pkg/detector/ospkg/alma/alma.go (Detect): a modular package without its modularitylabel
	func(p packageurl.PackageURL, system string) bool {
		return system == "alma" && p.Type == packageurl.TypeRPM && strings.Contains(p.Version, ".module_el") && p.Qualifiers.Map()["modularitylabel"] == ""
	},
}

func osPurl(p packageurl.PackageURL) bool {
	return p.Type == packageurl.TypeDebian || p.Type == packageurl.TypeRPM || p.Type == packageurl.TypeApk
}

// unscanned is the bom-ref of every component of a part whole has passed whose purl trivy lists but never
// matches, in the part's order: a type or distribution it has no driver for, or a package one of its
// detectors skips (trivySkips). The server counts each as not checked.
func unscanned(part []byte) []string {
	var doc struct{ Components []component }
	_ = json.Unmarshal(part, &doc) // whole read it already
	system := ""
	for _, c := range doc.Components {
		if c.Type == "operating-system" {
			system = c.Name
		}
	}
	var refs []string
	for _, c := range doc.Components {
		p, err := packageurl.FromString(c.Purl)
		switch {
		case c.Purl == "" || err != nil: // no purl: a note the server counts already; one that does not parse whole refused
		case osPurl(p) && !slices.Contains(osDrivers, system), !osPurl(p) && !slices.Contains(scannedTypes, p.Type),
			slices.ContainsFunc(trivySkips, func(skips func(packageurl.PackageURL, string) bool) bool { return skips(p, system) }):
			refs = append(refs, c.BOMRef)
		}
	}
	return refs
}

// considered is nil when trivy's report lists, among the packages it read (--list-all-pkgs), every
// component of the part that carries a purl, by the pair of its bom-ref and its purl, both purls as
// packageurl-go normalises them — as trivy does — so trivy's spelling of a purl refuses nothing: a package
// trivy skipped was never matched, so the answer is not a clean one, nor one whose bom-ref trivy read for
// another purl.
func considered(part, report []byte) error {
	var doc struct{ Components []component }
	var r struct {
		Results []struct {
			Packages []struct{ Identifier struct{ BOMRef, PURL string } }
		}
	}
	if err := json.Unmarshal(part, &doc); err != nil {
		return err
	}
	if err := json.Unmarshal(report, &r); err != nil {
		return err
	}
	canonical := func(purl string) (string, bool) {
		p, err := packageurl.FromString(purl)
		return p.String(), err == nil
	}
	read := map[[2]string]bool{}
	for _, res := range r.Results {
		for _, p := range res.Packages {
			if purl, ok := canonical(p.Identifier.PURL); ok {
				read[[2]string{p.Identifier.BOMRef, purl}] = true
			}
		}
	}
	var unread []string
	for _, c := range doc.Components {
		if purl, ok := canonical(c.Purl); c.Purl != "" && (!ok || !read[[2]string{c.BOMRef, purl}]) {
			unread = append(unread, c.BOMRef)
		}
	}
	if len(unread) > 0 {
		return fmt.Errorf("trivy read none of %d components: %s", len(unread), strings.Join(unread, ", "))
	}
	return nil
}
