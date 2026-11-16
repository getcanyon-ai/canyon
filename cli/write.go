package cli

import (
	"os"
	"path/filepath"
)

// atomicWrite is the one way this binary puts bytes at a path it may later read back: a temp file
// beside the target, 0600 and O_EXCL from os.CreateTemp, written whole, then renamed over it. A
// crash mid-write leaves the old file as it was, and O_CREATE|O_TRUNC on the real path would have
// left an existing 0644 file at 0644, since the mode argument only applies to a file being created.
// atomicWrite is temp-and-rename beside the target: a crash mid-write leaves the old file whole.
// A new file is private (0600, CreateTemp's own mode). For a file that exists, private says
// whether it is made 0600 whatever it was (the config, which holds a session: always) or keeps
// the mode it had (a committed AGENTS.md is the repo's, and a rewrite is not the moment to
// make it private).
func atomicWrite(path string, body []byte, private bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() // a no-op once the rename has succeeded
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil && !private {
		if err := os.Chmod(f.Name(), info.Mode().Perm()); err != nil {
			return err
		}
	}
	return os.Rename(f.Name(), path)
}
