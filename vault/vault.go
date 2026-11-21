// Package vault seals secret values at rest: each value under its own data key, each data key
// under the key file, both bound to the row the value belongs to, so a ciphertext copied into
// another row does not open (design/ARCHITECTURE.md; contracts/routes.md, the secrets rows).
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const keySize = 32

// Overhead is what sealing adds to a value: the nonce and the tag.
const Overhead = 28

// Vault seals under the newest key file's key and opens under the one a row names.
type Vault struct {
	path   string         // the key file: version N is path.N beside it
	mu     sync.Mutex     // guards keys: Open adds a version a rotation wrote after Load
	keys   map[int][]byte // every key file's key by version: 1 is the key file, N is kek.N beside it
	active int            // the highest version present: what Seal uses
	// Generated is true when Load wrote the key file itself: a first boot — or a key volume
	// that failed to mount, which the caller tells apart by whether sealed rows already exist.
	Generated bool
}

// Load reads the 32-byte key at path as version 1, generating one (mode 0600) when the file
// is absent, then every later version beside it: a file named like the key file with ".N"
// appended, N an integer from 2, in any order and with any gaps. A file of any other length
// is refused, whatever its version: it is not a key this vault wrote. Load runs once, at
// boot; a second process racing the first boot loses the create and reads the winner's key.
// Load reads version 1 from path and every kek.N beside it; the highest present is the active
// version. A key file is generated only on a true first boot — no key file of any version on the
// volume. With kek.N present and version 1 gone, nothing is generated: what exists is loaded and
// the boot decides whether a row still needs version 1 (refuseRowsUnderAMissingKey).
func Load(path string) (*Vault, error) {
	v := &Vault{path: path, keys: map[int][]byte{}}
	key, err := readKey(path)
	switch {
	case err == nil:
		v.keys[1], v.active = key, 1
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		suffix, versioned := strings.CutPrefix(e.Name(), filepath.Base(path)+".")
		n, err := strconv.Atoi(suffix)
		if !versioned || err != nil || n < 2 || strconv.Itoa(n) != suffix {
			continue // not a version: whatever else the operator keeps beside the key file
		}
		key, err := readKey(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		v.keys[n] = key
		v.active = max(v.active, n)
	}
	if len(v.keys) == 0 {
		return generateFirst(path)
	}
	return v, nil
}

func readKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path) //nolint:gosec // the operator's key file
	if err != nil {
		return nil, err
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("%s holds %d bytes; a key file holds %d", path, len(key), keySize)
	}
	return key, nil
}

// generateFirst writes version 1 on a true first boot: 32 random bytes at path, 0600, O_EXCL.
func generateFirst(path string) (*Vault, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	key, err := writeKey(path)
	if errors.Is(err, os.ErrExist) {
		return Load(path) // the other booter won the create
	}
	if err != nil {
		return nil, err
	}
	return &Vault{path: path, keys: map[int][]byte{1: key}, active: 1, Generated: true}, nil
}

// writeKey writes 32 random bytes to a new file at path, 0600, O_EXCL: never over an existing key.
func writeKey(path string) ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator's key file
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		return nil, err
	}
	return key, f.Close()
}

// AddVersion writes kek.N beside the key file at path, N one above the active version, and makes
// it active. It never overwrites: a file already at that name fails the call.
func (v *Vault) AddVersion(path string) (int, error) {
	next := v.active + 1
	key, err := writeKey(path + "." + strconv.Itoa(next))
	if err != nil {
		return 0, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys[next], v.active = key, next
	return next, nil
}

// key is version's key: loaded at boot, or read from kek.N the first time a row names it (a
// rotation beside this process wrote it after Load) and kept. No file answers nil, no error.
func (v *Vault) key(version int) ([]byte, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[version]; ok {
		return key, nil
	}
	if version < 2 {
		return nil, nil
	}
	key, err := readKey(v.path + "." + strconv.Itoa(version))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.keys[version] = key
	return key, nil
}

func (v *Vault) Seal(project, env, name string, value []byte) (version int, sealedKey, sealedValue []byte, err error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return 0, nil, nil, err
	}
	row := aad(project, env, name)
	if sealedValue, err = seal(key, row, value); err != nil {
		return 0, nil, nil, err
	}
	activeKey, err := v.key(v.active)
	if err != nil {
		return 0, nil, nil, err
	}
	if sealedKey, err = seal(activeKey, row, key); err != nil {
		return 0, nil, nil, err
	}
	return v.active, sealedKey, sealedValue, nil
}

// Open is the inverse of Seal for the same row under the key version Seal answered; any other
// row, another version, or a changed byte fails, and the error names the row and the stage —
// never a value, never a key.
func (v *Vault) Open(version int, project, env, name string, sealedKey, sealedValue []byte) ([]byte, error) {
	fileKey, err := v.key(version)
	if err != nil {
		return nil, fmt.Errorf("open %s/%s/%s: %w", project, env, name, err)
	}
	if fileKey == nil {
		return nil, fmt.Errorf("open %s/%s/%s: key version %d is not loaded", project, env, name, version)
	}
	row := aad(project, env, name)
	key, err := open(fileKey, row, sealedKey)
	if err != nil {
		return nil, fmt.Errorf("open %s/%s/%s: the data key: %w", project, env, name, err)
	}
	value, err := open(key, row, sealedValue)
	if err != nil {
		return nil, fmt.Errorf("open %s/%s/%s: the value: %w", project, env, name, err)
	}
	return value, nil
}

// Rewrap re-seals a row's data key, opened under the version the row names, under the active
// version, and answers both. The value's ciphertext is not an input: the value never moves.
func (v *Vault) Rewrap(version int, project, env, name string, sealedKey []byte) (int, []byte, error) {
	fileKey, err := v.key(version)
	if err != nil {
		return 0, nil, fmt.Errorf("rewrap %s/%s/%s: %w", project, env, name, err)
	}
	if fileKey == nil {
		return 0, nil, fmt.Errorf("rewrap %s/%s/%s: key version %d is not loaded", project, env, name, version)
	}
	row := aad(project, env, name)
	key, err := open(fileKey, row, sealedKey)
	if err != nil {
		return 0, nil, fmt.Errorf("rewrap %s/%s/%s: the data key: %w", project, env, name, err)
	}
	activeKey, err := v.key(v.active)
	if err != nil {
		return 0, nil, err
	}
	resealed, err := seal(activeKey, row, key)
	return v.active, resealed, err
}

// BuildDigest is HMAC-SHA256 over the project, values sorted by name, the image they were built into
// and the builder that built it (contracts/executor.md, GET /builder), each part length-prefixed, under a key HKDF derives from the active key file's (#1244,
// contracts/routes.md, the deploys row): the same values give the same digest for the same image while
// the key stays, another image never does, and without the key no value, however short, can be guessed
// back from it. Hex; the values stay in memory.
func (v *Vault) BuildDigest(project, image, builder string, values map[string]string) (string, error) {
	v.mu.Lock()
	active := v.active
	v.mu.Unlock()
	fileKey, err := v.key(active)
	if err != nil {
		return "", err
	}
	if fileKey == nil {
		return "", fmt.Errorf("build digest: key version %d is not loaded", active)
	}
	key, err := hkdf.Key(sha256.New, fileKey, nil, "canyon build digest", keySize)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	part := func(s string) { _, _ = fmt.Fprintf(mac, "%d:%s", len(s), s) }
	part(project)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		part(name)
		part(values[name])
	}
	part(image)
	part(builder)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// ValuesHMAC is HMAC-SHA256 over the project and the values sorted by name alone, no image and no
// builder, each part length-prefixed, under a key HKDF derives from the active key file's with a label
// of its own (design/PATCHING.md §5, contracts/routes.md, the deploys row): two builds' values compare
// equal under one key version without either being read back. Hex, and the version it was made under.
func (v *Vault) ValuesHMAC(project string, values map[string]string) (string, int, error) {
	v.mu.Lock()
	active := v.active
	v.mu.Unlock()
	fileKey, err := v.key(active)
	if err != nil {
		return "", 0, err
	}
	if fileKey == nil {
		return "", 0, fmt.Errorf("values hmac: key version %d is not loaded", active)
	}
	key, err := hkdf.Key(sha256.New, fileKey, nil, "canyon build values", keySize)
	if err != nil {
		return "", 0, err
	}
	mac := hmac.New(sha256.New, key)
	part := func(s string) { _, _ = fmt.Fprintf(mac, "%d:%s", len(s), s) }
	part(project)
	for _, name := range slices.Sorted(maps.Keys(values)) {
		part(name)
		part(values[name])
	}
	return hex.EncodeToString(mac.Sum(nil)), active, nil
}

// Holder is HMAC-SHA256 of a session under a key HKDF derives from the active key file's for one 30-day period since
// the Unix epoch (contracts/routes.md, the playbooks row; design/specs/mcp.md §3.5): one session is one holder within a
// period, the key is never stored, and a holder can't be joined back to its session. 32 bytes.
// ponytail: a session spanning a period boundary is two holders until the older rows age out.
func (v *Vault) Holder(session string, at time.Time) ([]byte, error) {
	v.mu.Lock()
	active := v.active
	v.mu.Unlock()
	fileKey, err := v.key(active)
	if err != nil {
		return nil, err
	}
	if fileKey == nil {
		return nil, fmt.Errorf("holder: key version %d is not loaded", active)
	}
	period := at.Unix() / int64(30*24*time.Hour/time.Second)
	key, err := hkdf.Key(sha256.New, fileKey, nil, fmt.Sprintf("canyon playbook syncs %d", period), keySize)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(session))
	return mac.Sum(nil), nil
}

// aad is the row as additional data, each field length-prefixed, so no two rows share it
// whatever their fields contain: the binding does not lean on the callers' name rules.
func aad(project, env, name string) []byte {
	return fmt.Appendf(nil, "%d:%s%d:%s%d:%s", len(project), project, len(env), env, len(name), name)
}

func seal(key, row, plain []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return g.Seal(nil, nil, plain, row), nil
}

func open(key, row, sealed []byte) ([]byte, error) {
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nil, sealed, row)
}

// gcm is AES-256-GCM with a random nonce prepended to every ciphertext by Seal and read back by
// Open — the one construction, from the standard library, no nonce bookkeeping of our own.
func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// Active is the version Seal wraps under: the highest key file present.
func (v *Vault) Active() int { return v.active }

// Has answers whether a key file for version was loaded: the version a row names opens only then.
func (v *Vault) Has(version int) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, ok := v.keys[version]
	return ok
}
