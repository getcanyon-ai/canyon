package vault

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func load(t *testing.T) *Vault {
	t.Helper()
	v, err := Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSealOpensOnlyForTheRowItWasSealedFor(t *testing.T) {
	v := load(t)
	value := []byte("line one\nline two") // lines are fine: a PEM key spans them
	_, sealedKey, sealed, err := v.Seal("claims-portal", "preview", "DATABASE_URL", value)
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != len(value)+Overhead {
		t.Fatalf("a sealed value is the value plus the overhead: %d bytes for %d", len(sealed), len(value))
	}
	if bytes.Contains(sealed, value) {
		t.Fatal("the sealed value carries the plaintext")
	}
	if bytes.Contains(sealedKey, value) {
		t.Fatal("the sealed key carries the plaintext")
	}
	got, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealed)
	if err != nil || !bytes.Equal(got, value) {
		t.Fatalf("round trip: %q %v", got, err)
	}
	for _, row := range [][3]string{{"other-app", "preview", "DATABASE_URL"}, {"claims-portal", "prod", "DATABASE_URL"}, {"claims-portal", "preview", "STRIPE_KEY"}} {
		if _, err := v.Open(1, row[0], row[1], row[2], sealedKey, sealed); err == nil {
			t.Fatalf("a ciphertext copied into row %v opened", row)
		}
	}
	_, sealedKey2, sealed2, err := v.Seal("a", "b|c", "d", value) // the same characters split differently are a different row
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Open(1, "a|b", "c", "d", sealedKey2, sealed2); err == nil {
		t.Fatal("two rows spelled alike shared a binding")
	}
	flipped := bytes.Clone(sealed)
	flipped[len(flipped)/2] ^= 1
	if _, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, flipped); err == nil || !strings.Contains(err.Error(), "claims-portal/preview/DATABASE_URL: the value") {
		t.Fatalf("a changed byte of the value fails naming the row and the stage: %v", err)
	}
	flipped = bytes.Clone(sealedKey)
	flipped[len(flipped)/2] ^= 1
	if _, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", flipped, sealed); err == nil || !strings.Contains(err.Error(), "claims-portal/preview/DATABASE_URL: the data key") {
		t.Fatalf("a changed byte of the data key fails naming the row and the stage: %v", err)
	}
	if _, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealed[:Overhead-1]); err == nil {
		t.Fatal("a ciphertext shorter than the overhead opened")
	}
	if _, err := load(t).Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealed); err == nil {
		t.Fatal("another key file opened")
	}
}

func TestSealHoldsEveryValueSizeTheContractAllows(t *testing.T) {
	v := load(t)
	for _, size := range []int{0, 1, 64 << 10} { // empty, one byte, the contract's 65,536-byte cap
		value := bytes.Repeat([]byte("x"), size)
		_, sealedKey, sealed, err := v.Seal("claims-portal", "preview", "DATABASE_URL", value)
		if err != nil || len(sealed) != size+Overhead {
			t.Fatalf("%d bytes: sealed to %d (%v)", size, len(sealed), err)
		}
		if got, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealed); err != nil || !bytes.Equal(got, value) {
			t.Fatalf("%d bytes: round trip %d (%v)", size, len(got), err)
		}
	}
}

func TestSealDrawsAFreshDataKeyEveryWrite(t *testing.T) {
	v := load(t)
	_, sealedKey1, _, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, sealed2, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("v2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey1, sealed2); err == nil {
		t.Fatal("one write's data key opened another write's value")
	}
}

func TestLoadGeneratesTheKeyOnceAndRefusesAForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "kek")
	first, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Generated {
		t.Fatal("the first boot generated the key and must say so")
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != keySize || info.Mode().Perm() != 0o600 {
		t.Fatalf("the generated key file: %v %v", info, err)
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("the key directory: %v %v", dir, err)
	}
	_, sealedKey, sealed, err := first.Seal("claims-portal", "preview", "DATABASE_URL", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.Generated {
		t.Fatal("a second boot read the key and must not claim to have generated it")
	}
	if got, err := again.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealed); err != nil || string(got) != "v" {
		t.Fatalf("a second boot reads the same key: %q %v", got, err)
	}
	if err := os.WriteFile(path, make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "16 bytes") {
		t.Fatalf("a 16-byte file is not a key file: %v", err)
	}
	if err := os.WriteFile(path, sealedKey[:keySize], 0o600); err != nil { // any 32 bytes: the key file is readable again
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".2", make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "kek.2 holds 16 bytes") {
		t.Fatalf("a 16-byte kek.2 is not a key file either: %v", err)
	}
}

// addKey writes version n beside the key file the way rotate-kek will: 32 random bytes.
func addKey(t *testing.T, path string, n int) {
	t.Helper()
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"."+strconv.Itoa(n), key, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSealUsesTheNewestKeyAndOpenUsesTheRowsOwn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	one, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	version, sealedKey1, sealed1, err := one.Seal("claims-portal", "preview", "DATABASE_URL", []byte("before"))
	if err != nil || version != 1 {
		t.Fatalf("the key file alone is version 1: %d %v", version, err)
	}
	addKey(t, path, 2)
	addKey(t, path, 5)  // gaps are fine: the highest present is the active one
	addKey(t, path, 10) // and highest by number, not by name: "kek.10" sorts before "kek.5"
	if err := os.Remove(filepath.Join(filepath.Dir(path), "kek.10")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kek.bak", "kek.02", "kek.1"} { // not versions, whatever they hold
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), make([]byte, 7), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	two, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if two.Generated {
		t.Fatal("a boot with later key files generated one")
	}
	version, sealedKey5, sealed5, err := two.Seal("claims-portal", "preview", "DATABASE_URL", []byte("after"))
	if err != nil || version != 5 {
		t.Fatalf("Seal uses the newest key: version %d %v", version, err)
	}
	if got, err := two.Open(5, "claims-portal", "preview", "DATABASE_URL", sealedKey5, sealed5); err != nil || string(got) != "after" {
		t.Fatalf("a row opens under its own version: %q %v", got, err)
	}
	if got, err := two.Open(1, "claims-portal", "preview", "DATABASE_URL", sealedKey1, sealed1); err != nil || string(got) != "before" {
		t.Fatalf("a row sealed before the rotation still opens under version 1: %q %v", got, err)
	}
	for _, v := range []int{1, 2} {
		if _, err := two.Open(v, "claims-portal", "preview", "DATABASE_URL", sealedKey5, sealed5); err == nil || !strings.Contains(err.Error(), "the data key") {
			t.Fatalf("a row sealed under 5 does not open under %d: %v", v, err)
		}
	}
	if _, err := two.Open(3, "claims-portal", "preview", "DATABASE_URL", sealedKey5, sealed5); err == nil || !strings.Contains(err.Error(), "claims-portal/preview/DATABASE_URL: key version 3 is not loaded") {
		t.Fatalf("a version no file holds is refused by name: %v", err)
	}
	if _, err := one.Open(3, "claims-portal", "preview", "DATABASE_URL", sealedKey5, sealed5); err == nil || !strings.Contains(err.Error(), "key version 3 is not loaded") {
		t.Fatalf("the boot before the rotation finds no file for 3 either: %v", err)
	}
}

// The envelope on the wire is the standard one and unchanged by versioning, so every row sealed
// before this store opens under version 1: AES-256-GCM, a 12-byte nonce in front of each
// ciphertext, a 16-byte tag behind it, the row as additional data spelled length-prefixed.
// Opened here with nothing but the standard library and the key file's bytes.
// Version 1 is generated only when no key file of any version exists. With kek.2 alone — the
// operator removed version 1 after a rotation — nothing is generated: 2 is loaded and active,
// 1 is absent, and the boot decides whether any row still needs it.
func TestLoadGeneratesNothingWhenALaterKeyFileExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	addKey(t, path, 2)
	v, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if v.Generated || v.Has(1) || !v.Has(2) || v.Active() != 2 {
		t.Fatalf("kek.2 alone: generated %v, has 1 %v, has 2 %v, active %d", v.Generated, v.Has(1), v.Has(2), v.Active())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key file was written beside kek.2: %v", err)
	}
	if version, _, _, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("x")); err != nil || version != 2 {
		t.Fatalf("sealed under %d (%v), want 2", version, err)
	}
	if _, err := v.Open(1, "claims-portal", "preview", "DATABASE_URL", nil, nil); err == nil || !strings.Contains(err.Error(), "key version 1 is not loaded") {
		t.Fatalf("version 1 is absent, by name: %v", err)
	}
}

func TestTheWireFormatIsTheStandardEnvelope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	v, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_, sealedKey, sealedValue, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	fileKey, err := os.ReadFile(path) //nolint:gosec // the test's own key file
	if err != nil {
		t.Fatal(err)
	}
	row := []byte("13:claims-portal7:preview12:DATABASE_URL")
	unwrap := func(key, sealed []byte) []byte {
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		g, err := cipher.NewGCM(block)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := g.Open(nil, sealed[:12], sealed[12:], row)
		if err != nil {
			t.Fatalf("not the standard envelope: %v", err)
		}
		return plain
	}
	if got := unwrap(unwrap(fileKey, sealedKey), sealedValue); string(got) != "v" || len(sealedValue) != 1+12+16 {
		t.Fatalf("the envelope: %q, %d bytes for one", got, len(sealedValue))
	}
}

// The highest version is by number: with kek.5 and kek.10 present, 10 is active, though "kek.10"
// sorts before "kek.5" in a directory listing.
func TestTheActiveVersionIsTheHighestByNumberNotByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	addKey(t, path, 5)
	addKey(t, path, 10)
	v, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if version, _, _, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("x")); err != nil || version != 10 {
		t.Fatalf("with kek.5 and kek.10: sealed under %d (%v), want 10", version, err)
	}
}

// Two seals of one row under one key never share a nonce: the nonce is what keeps AES-GCM safe
// under a key that wraps many values.
func TestTwoSealsOfOneRowNeverShareANonce(t *testing.T) {
	v, err := Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	_, key1, val1, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	_, key2, val2, err := v.Seal("claims-portal", "preview", "DATABASE_URL", []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(key1[:12], key2[:12]) || bytes.Equal(val1[:12], val2[:12]) {
		t.Fatal("two seals of one row share a nonce")
	}
}

// AddVersion writes the next version with O_EXCL: a file already at that name is never overwritten.
func TestAddVersionNeverOverwritesAKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	v, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	addKey(t, path, 2)
	before, err := os.ReadFile(path + ".2") //nolint:gosec // the test's own key file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.AddVersion(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("a kek.2 already on the volume refuses the write: %v", err)
	}
	if after, err := os.ReadFile(path + ".2"); err != nil || !bytes.Equal(before, after) { //nolint:gosec // the test's own key file
		t.Fatalf("kek.2 changed: %v", err)
	}
	if v.Active() != 1 {
		t.Fatalf("a refused write leaves the active version: %d", v.Active())
	}
}

// A rotation beside the server writes kek.2 after the server's boot; the server opens rows under it
// without a restart, reading kek.2 the first time a row names it, and still seals under 1.
func TestOpenReadsAVersionWrittenAfterLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kek")
	server, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotation.AddVersion(path); err != nil {
		t.Fatal(err)
	}
	version, sealedKey, sealed, err := rotation.Seal("claims-portal", "preview", "DATABASE_URL", []byte("rotated"))
	if err != nil || version != 2 {
		t.Fatalf("sealed under the new version: %d %v", version, err)
	}
	if got, err := server.Open(2, "claims-portal", "preview", "DATABASE_URL", sealedKey, sealed); err != nil || string(got) != "rotated" {
		t.Fatalf("the server opens a row under a version written after its boot: %q %v", got, err)
	}
	if version, _, _, err := server.Seal("claims-portal", "preview", "DATABASE_URL", []byte("v")); err != nil || version != 1 {
		t.Fatalf("the server still seals under the version it booted with: %d %v", version, err)
	}
}

// The build digest is keyed (#1244): the same values under another key file give another digest, so a
// short value cannot be guessed back from a stored one without the key. It reads the values as a set:
// whatever order they come in, one digest; a byte moved from a name into its value is another set; and
// it is bound to its project, its image and its builder, so neither another app's row, a row whose image
// changed under it, nor an image a patched builder would build differently matches.
func TestTheBuildDigestIsKeyedBoundAndReadsTheValuesAsASet(t *testing.T) {
	const app, image, builder = "claims-portal", "canyon/claims-portal@sha256:aa", "sha256:bb"
	values := map[string]string{}
	for i := range 32 {
		values["NAME_"+strconv.Itoa(i)] = "value-" + strconv.Itoa(i)
	}
	one, err := load(t).BuildDigest(app, image, builder, values)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := load(t).BuildDigest(app, image, builder, values); err != nil || other == one {
		t.Fatalf("another key, the same digest: %s (%v)", other, err)
	}
	v := load(t)
	first, err := v.BuildDigest(app, image, builder, values)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 { // a map ranges in a random order each time
		if again, err := v.BuildDigest(app, image, builder, maps.Clone(values)); err != nil || again != first {
			t.Fatalf("the same values, another order, another digest: %s != %s (%v)", again, first, err)
		}
	}
	a, _ := v.BuildDigest(app, image, builder, map[string]string{"A": "BC"})
	b, _ := v.BuildDigest(app, image, builder, map[string]string{"AB": "C"})
	if a == b || len(a) != 64 {
		t.Fatalf("a byte moved across the name's end is the same digest, or not 32 bytes hex: %s %s", a, b)
	}
	for _, d := range [][3]string{{"quote-engine", image, builder}, {app, "canyon/claims-portal@sha256:bb", builder}, {app, image, "sha256:cc"}} {
		if other, err := v.BuildDigest(d[0], d[1], d[2], values); err != nil || other == first {
			t.Fatalf("%s's %s by %s: the same digest (%v)", d[0], d[1], d[2], err)
		}
	}
}

// The values-only HMAC (design/PATCHING.md §5) reads the values as a set and names the key version it was
// made under, so two builds compare under one version only; it is bound to its project, never to an image
// or a builder, and its key is its own: it is never the build digest of the same values.
func TestTheValuesHMACIsKeyedVersionedAndReadsTheValuesAlone(t *testing.T) {
	const app = "claims-portal"
	values := map[string]string{"NEXT_PUBLIC_API": "https://api.example", "NODE_ENV": "production"}
	v := load(t)
	first, version, err := v.ValuesHMAC(app, values)
	if err != nil || version != v.Active() || len(first) != 64 {
		t.Fatalf("%s under version %d, active %d (%v)", first, version, v.Active(), err)
	}
	for range 20 {
		if again, _, err := v.ValuesHMAC(app, maps.Clone(values)); err != nil || again != first {
			t.Fatalf("the same values, another order, another hmac: %s != %s (%v)", again, first, err)
		}
	}
	if other, _, err := load(t).ValuesHMAC(app, values); err != nil || other == first {
		t.Fatalf("another key, the same hmac: %s (%v)", other, err)
	}
	if other, _, _ := v.ValuesHMAC("quote-engine", values); other == first {
		t.Fatal("another project, the same hmac")
	}
	if digest, _ := v.BuildDigest(app, "", "", values); digest == first {
		t.Fatal("the values hmac is the build digest of an empty image and builder: one key for both")
	}
}
