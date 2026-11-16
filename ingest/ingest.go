package ingest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

type Package struct {
	ID       string `json:"id"`        // pkg_<tree hash prefix>
	TreeHash string `json:"tree_hash"` // the git tree the package is
	Commit   string `json:"commit"`    // the commit holding it in the project's repo
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
}

var ErrRequest = errors.New("bad package")

const (
	maxBytes    = 512 << 20
	maxEntries  = 100000
	sparseSlack = 1 << 20
)

// A folder's archive is read past the folder up to these, whatever the folder holds (#1917): a test lowers them.
var maxArchiveBytes, maxArchiveEntries int64 = 4 << 30, 1000000

// Ingest turns the uploaded tar into a commit on main of the project's bare repository at
// repo (the caller has validated the project id the path is built from) and returns the
// package it became. exclusions are the snapshot's segment globs. dropRoot is Untar's.
func Ingest(ctx context.Context, repo string, tr io.Reader, exclusions []string, dropRoot bool) (Package, error) {
	return IngestChecked(ctx, repo, tr, exclusions, dropRoot, true, nil)
}

// IngestChecked is Ingest whose check reads the unpacked tree of a new package before it is
// committed: its error is returned as it is, and no ref moves. An existing package is not checked.
// moveMain false commits the package onto main without moving it: a pull request's head, reviewed
// and never the project's current package.
func IngestChecked(ctx context.Context, repo string, tr io.Reader, exclusions []string, dropRoot, moveMain bool, check func(tree string) error) (Package, error) {
	return ingest(ctx, repo, tr, exclusions, dropRoot, "", moveMain, check)
}

// IngestFolder is IngestChecked of one folder of a repository's tarball, its root dropped: only the
// entries under folder are the package, at its root, and the caps count them alone (#1917).
func IngestFolder(ctx context.Context, repo string, tr io.Reader, exclusions []string, folder string, moveMain bool, check func(tree string) error) (Package, error) {
	return ingest(ctx, repo, tr, exclusions, true, folder, moveMain, check)
}

func ingest(ctx context.Context, repo string, tr io.Reader, exclusions []string, dropRoot bool, folder string, moveMain bool, check func(tree string) error) (Package, error) {
	for _, pat := range exclusions {
		if _, err := path.Match(pat, ""); err != nil { // fail closed: a bad glob must not quietly exclude nothing
			return Package{}, fmt.Errorf("exclusion %q: %w", pat, err)
		}
	}
	if err := ensureRepo(ctx, repo); err != nil {
		return Package{}, err
	}
	work, err := os.MkdirTemp("", "canyon-ingest-")
	if err != nil {
		return Package{}, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	tree, index := filepath.Join(work, "tree"), filepath.Join(work, "index") // git's index never sits inside the upload
	if err := os.Mkdir(tree, 0o750); err != nil {
		return Package{}, err
	}
	pkg, err := untar(tr, tree, exclusions, dropRoot, folder, 0o600, 0o750)
	if err != nil {
		return Package{}, err
	}
	env := "GIT_INDEX_FILE=" + index
	if out, err := git(ctx, repo, env, "--work-tree="+tree, "add", "-A", "-f", "--", "."); err != nil { // -f: an uploaded .gitignore hides nothing from the gate
		return Package{}, fmt.Errorf("git add: %w: %s", err, out)
	}
	hash, err := git(ctx, repo, env, "write-tree")
	if err != nil {
		return Package{}, fmt.Errorf("git write-tree: %w: %s", err, hash)
	}
	pkg.TreeHash = strings.TrimSpace(hash)
	commitMu.Lock() // the dedupe check and the commit are one step: two uploads of one tree must see each other
	defer commitMu.Unlock()
	if existing, err := git(ctx, repo, "", "rev-parse", "--verify", "-q", "refs/canyon/trees/"+pkg.TreeHash); err == nil {
		pkg.Commit = strings.TrimSpace(existing)
		id, err := git(ctx, repo, "", "for-each-ref", "--format=%(refname:short)", "--points-at="+pkg.TreeHash, "refs/canyon/ids/")
		if pkg.ID = strings.TrimPrefix(strings.TrimSpace(id), "canyon/ids/"); err != nil || pkg.ID == "" {
			return Package{}, fmt.Errorf("git for-each-ref: package id for tree %s: %v: %s", pkg.TreeHash[:12], err, id)
		}
		return pkg, nil
	}
	if check != nil {
		if err := check(tree); err != nil {
			return Package{}, err
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		parent, _ := git(ctx, repo, "", "rev-parse", "--verify", "-q", "main")
		parent = strings.TrimSpace(parent)
		args := []string{"commit-tree", pkg.TreeHash, "-m", "package " + pkg.TreeHash[:12]}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		commit, err := git(ctx, repo, "GIT_AUTHOR_NAME=canyon\x00GIT_AUTHOR_EMAIL=canyon@localhost\x00GIT_COMMITTER_NAME=canyon\x00GIT_COMMITTER_EMAIL=canyon@localhost", args...)
		if err != nil {
			return Package{}, fmt.Errorf("git commit-tree: %w: %s", err, commit)
		}
		pkg.Commit = strings.TrimSpace(commit)
		if moveMain {
			if out, err := git(ctx, repo, "", "update-ref", "refs/heads/main", pkg.Commit, parent); err != nil {
				if head, _ := git(ctx, repo, "", "rev-parse", "--verify", "-q", "main"); strings.TrimSpace(head) != parent {
					continue // main moved under us (another process): commit onto the new head
				}
				return Package{}, fmt.Errorf("git update-ref: %w: %s", err, out)
			}
		}
		pkg.ID = packageID(ctx, repo, pkg.TreeHash)
		for _, ref := range [][2]string{{"refs/canyon/ids/" + pkg.ID, pkg.TreeHash}, {"refs/canyon/trees/" + pkg.TreeHash, pkg.Commit}} { // id first: whoever finds the tree ref finds the id
			if out, err := git(ctx, repo, "", "update-ref", ref[0], ref[1]); err != nil {
				return Package{}, fmt.Errorf("git update-ref %s: %w: %s", ref[0], err, out)
			}
		}
		return pkg, nil
	}
	return Package{}, errors.New("git update-ref: main kept moving; give up after five attempts")
}

func packageID(ctx context.Context, repo, tree string) string {
	for n := 6; n <= len(tree); n += 2 {
		id := "pkg_" + tree[:n]
		taken, err := git(ctx, repo, "", "rev-parse", "--verify", "-q", "refs/canyon/ids/"+id)
		if err != nil || strings.TrimSpace(taken) == tree {
			return id
		}
	}
	return "pkg_" + tree
}

// archive is git archive of the tree at commit, as a tar stream on stdout.
func archive(ctx context.Context, repo, commit string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", "--git-dir="+repo, "archive", "--format=tar", commit) //nolint:gosec // git on our own repo dir with a commit our own tables gave us
	cmd.Env = gitEnv()
	return cmd
}

// Archive writes the tree at commit to w as a tar stream: canyon pull. Bytes may have reached w
// before an error, so the caller has already answered by then; the reader's tar sees the cut.
func Archive(ctx context.Context, repo, commit string, w io.Writer) error {
	cmd := archive(ctx, repo, commit)
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git archive %s: %w", commit, err)
	}
	return nil
}

// Extract materialises the tree at commit into dir, for the gate to read: git archive piped
// through the same untar the upload went through (its pax header is skipped, nothing escapes).
func Extract(ctx context.Context, repo, commit, dir string) error {
	cmd := archive(ctx, repo, commit)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	_, untarErr := Untar(out, dir, nil, false)
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("git archive %s: %w", commit, err)
	}
	return untarErr
}

// RootNames is the names of the tree's top-level entries at commit: a run read's facts
// (contracts/routes.md, What a starter is missing).
func RootNames(ctx context.Context, repo, commit string) ([]string, error) {
	out, err := git(ctx, repo, "", "ls-tree", "-z", "--name-only", commit)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w: %s", commit, err, out)
	}
	if out == "" {
		return nil, nil // an empty tree
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

func gitEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C"} // git sees nothing of the server's environment
}

func git(ctx context.Context, repo, extraEnv string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + repo}, args...)...) //nolint:gosec // git on our own repo dir with fixed subcommands
	cmd.Env = gitEnv()
	if extraEnv != "" {
		cmd.Env = append(cmd.Env, strings.Split(extraEnv, "\x00")...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Excluded reports whether any segment of the slash path p is excluded (contracts/snapshot.md,
// exclusions): an entry matches one segment at any depth, one starting with / the first segment alone,
// and one starting with ! lets through a segment another matched. Exported because the CLI applies the
// same list to the same paths before it uploads them (cli/pack.go): one matcher, both sides.
func Excluded(p string, exclusions []string) bool {
	for i, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".git") || strings.EqualFold(seg, ".gitattributes") || strings.EqualFold(seg, ".gitmodules") {
			return true // git's own config never travels, in any letter case: an upload does not get to steer git
		}
		matched, kept := false, false
		for _, pat := range exclusions {
			keep := strings.HasPrefix(pat, "!")
			pat = strings.TrimPrefix(pat, "!")
			if root := strings.HasPrefix(pat, "/"); root && i > 0 {
				continue
			}
			if ok, _ := path.Match(strings.TrimPrefix(pat, "/"), seg); ok {
				matched, kept = matched || !keep, kept || keep
			}
		}
		if matched && !kept {
			return true
		}
	}
	return false
}

// For a client (canyon pull) the returned Package carries Files and Bytes only: ID, TreeHash and
// Commit are the server's, minted by Ingest, and read as zero here.
// Untar writes the archive into dir and reports what it wrote. Exported because canyon pull unpacks
// with it (cli/cmd_pull.go): one set of caps and refusals both ways, and no second unpacker to drift.
// dropRoot unwraps an archive that holds one top-level directory (a GitHub tarball's owner-repo-sha/):
// the first entry names that directory, every entry must sit under it, and it is stripped from each.
func Untar(r io.Reader, dir string, exclusions []string, dropRoot bool) (Package, error) {
	return untar(r, dir, exclusions, dropRoot, "", 0o600, 0o750)
}

// UntarWorkTree is Untar into a person's working tree, as canyon pull writes it: each file 0644, or 0755
// with an executable bit, and each directory 0755 — the modes the CLI's pack sends — so a pull leaves a
// tree others on the machine read as a checkout does, where the server's own unpacking keeps them its own (#1250).
func UntarWorkTree(r io.Reader, dir string) (Package, error) {
	return untar(r, dir, nil, false, "", 0o644, 0o755)
}

// untar's folder, when not "", keeps only the entries under it, at the root: the rest are read past,
// never written and counted against the archive's own bounds alone, never the package's caps.
func untar(r io.Reader, dir string, exclusions []string, dropRoot bool, folder string, filePerm, dirPerm os.FileMode) (Package, error) {
	var pkg Package
	var top string
	var declared int64            // every regular entry of the package, kept or excluded: the cap bounds what it holds, not just what is written
	var archive, read int64       // every regular entry's bytes, and every entry, of a folder's whole archive
	root, err := os.OpenRoot(dir) // every write goes through the root: a symlink already in dir cannot lead a write outside it
	if err != nil {
		return pkg, err
	}
	defer func() { _ = root.Close() }()
	in := &countingReader{r: r}
	tr := tar.NewReader(in)
	entries := &expansion{r: tr, in: in}
	for n := 0; ; {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return pkg, nil
		}
		if err != nil {
			return pkg, fmt.Errorf("%w: %v", ErrRequest, err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader { // git archive writes one first
			continue
		}
		name := filepath.ToSlash(h.Name)
		clean := path.Clean(name)
		if clean == "." { // tar -cf x . writes the root itself
			continue
		}
		if path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") {
			return pkg, fmt.Errorf("%w: entry %q escapes the tree", ErrRequest, h.Name)
		}
		if dropRoot {
			first, rest, _ := strings.Cut(clean, "/")
			if top == "" {
				top = first
			}
			if first != top || (rest == "" && h.Typeflag != tar.TypeDir) {
				return pkg, fmt.Errorf("%w: entry %q is not under the archive's root %q", ErrRequest, h.Name, top+"/")
			}
			if rest == "" { // the root directory itself
				continue
			}
			clean = rest
		}
		if folder != "" {
			if read++; read > maxArchiveEntries {
				return pkg, fmt.Errorf("%w: the repository holds more than %d entries", ErrRequest, maxArchiveEntries)
			}
			if archive += h.Size; archive > maxArchiveBytes {
				return pkg, fmt.Errorf("%w: the repository holds more than %d bytes", ErrRequest, maxArchiveBytes)
			}
			rest, under := strings.CutPrefix(clean, folder+"/")
			if !under {
				if err := entries.copy(io.Discard, h); err != nil { // read past, as an excluded entry is
					return pkg, err
				}
				continue
			}
			clean = rest
		}
		if n++; n > maxEntries { // routes.md refuses a package *over* the cap: exactly maxEntries is taken, the next is refused before it is written
			return pkg, fmt.Errorf("%w: more than %d entries", ErrRequest, maxEntries)
		}
		if h.Typeflag == tar.TypeReg {
			if declared += h.Size; declared > maxBytes {
				return pkg, fmt.Errorf("%w: package exceeds %d bytes", ErrRequest, maxBytes)
			}
		}
		if Excluded(clean, exclusions) {
			if err := entries.copy(io.Discard, h); err != nil { // drained, never written, still not expanded
				return pkg, err
			}
			continue
		}
		target := filepath.FromSlash(clean) // relative to root, which refuses a path that leaves dir through a symlink
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(target, dirPerm); err != nil {
				return pkg, entryErr(h, err)
			}
		case tar.TypeReg:
			pkg.Bytes += h.Size
			if err := root.MkdirAll(filepath.Dir(target), dirPerm); err != nil {
				return pkg, entryErr(h, err)
			}
			mode := filePerm | os.FileMode(h.Mode&0o111) // masked before the conversion: three bits cannot overflow
			f, err := root.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return pkg, entryErr(h, err)
			}
			if err := f.Chmod(mode); err != nil { // O_CREATE keeps an existing file's mode; a re-pull must reproduce the package's
				_ = f.Close()
				return pkg, entryErr(h, err)
			}
			if err := entries.copy(f, h); err != nil {
				_ = f.Close()
				return pkg, err
			}
			if err := f.Close(); err != nil {
				return pkg, entryErr(h, err)
			}
			pkg.Files++
		default:
			return pkg, fmt.Errorf("%w: entry %q: only files and directories are accepted", ErrRequest, h.Name)
		}
	}
}

// entryErr reports a filesystem failure on an entry as the package's problem, naming the entry
// and not the directory. ponytail: a full disk reads as a bad package too;
// split by errno if that ever shows up.
func entryErr(h *tar.Header, err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("%w: entry %q: %v", ErrRequest, h.Name, err)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// expansion refuses an entry the moment more bytes have come out of the archive than went in:
// a sparse entry expands, a real one never does, and it is caught mid-entry rather than after
// the temp file or the discard loop has already paid for it.
type expansion struct {
	r   io.Reader
	in  *countingReader
	out int64
}

var errSparse = errors.New("sparse")

func (e *expansion) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if e.out += int64(n); e.out > e.in.n+sparseSlack {
		return n, errSparse
	}
	return n, err
}

func (e *expansion) copy(dst io.Writer, h *tar.Header) error {
	_, err := io.CopyN(dst, e, h.Size) //nolint:gosec // kept entries are bounded by maxBytes; every entry is bounded by the archive's real size through Read
	if err == nil && e.out > e.in.n+sparseSlack {
		err = errSparse // CopyN hides Read's error when the final read completed the entry
	}
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return nil
	case errors.Is(err, errSparse):
		return fmt.Errorf("%w: entry %q expands beyond the archive; sparse entries are refused", ErrRequest, h.Name)
	}
	return entryErr(h, err) // a write failure names the entry, never the server's temp path
}

var (
	initMu   sync.Mutex // two first uploads to one project must not race git init
	commitMu sync.Mutex // dedupe and commit to a project's main take turns within this process
)

func ensureRepo(ctx context.Context, repo string) error {
	initMu.Lock()
	defer initMu.Unlock()
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(repo, 0o750); err != nil {
		return err
	}
	if out, err := git(ctx, repo, "", "init", "--bare", "-q", "-b", "main", repo); err != nil {
		return fmt.Errorf("git init: %w: %s", err, out)
	}
	return nil
}
