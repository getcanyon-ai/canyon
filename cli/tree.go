package cli

import (
	"archive/tar"
	"bytes"
	"crypto/sha1" //nolint:gosec // git names its objects by SHA-1: this is a name, never a secret
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// treeHash is the name the host would give the package `canyon deploy` sends from dir: the git tree
// of that tar, as ingest.Ingest writes it with `git add -A -f` and `git write-tree`. The host's git
// reads no config and .gitattributes never travels, so a blob is its bytes, and a file is 100755
// when pack marks it executable, else 100644. A directory is only ever the path to a file, so one
// with no file under it never appears, as in git.
func treeHash(dir string) (string, error) {
	r, w := io.Pipe()
	defer func() { _ = r.Close() }() // a read that stops early ends the pack's writes too
	go func() {
		_, _, _, err := pack(dir, w)
		_ = w.CloseWithError(err)
	}()
	root := &gitNode{kids: map[string]*gitNode{}}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return hex.EncodeToString(root.tree()), nil
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		blob := sha1.New() //nolint:gosec // git's object name, as above
		_, _ = fmt.Fprintf(blob, "blob %d\x00", h.Size)
		if _, err := io.Copy(blob, tr); err != nil { //nolint:gosec // pack's own tar of dir, capped at maxPackBytes as it is written
			return "", err
		}
		mode := "100644"
		if h.Mode&0o111 != 0 {
			mode = "100755"
		}
		root.add(strings.Split(h.Name, "/"), &gitNode{mode: mode, sum: blob.Sum(nil)})
	}
}

// gitNode is a file (mode and blob name) or a directory (its entries by name).
type gitNode struct {
	mode string
	sum  []byte
	kids map[string]*gitNode // nil for a file
}

func (n *gitNode) add(path []string, file *gitNode) {
	for _, name := range path[:len(path)-1] {
		if n.kids[name] == nil {
			n.kids[name] = &gitNode{mode: "40000", kids: map[string]*gitNode{}}
		}
		n = n.kids[name]
	}
	n.kids[path[len(path)-1]] = file
}

// tree is n's git tree object name. Git sorts a directory's entries as bytes, a directory as if its
// name ended in "/": the directory "a" sorts after the files "a-b" and "a.b", where by name alone it
// would come first.
func (n *gitNode) tree() []byte {
	key := func(name string) string {
		if n.kids[name].kids != nil {
			return name + "/"
		}
		return name
	}
	names := slices.SortedFunc(maps.Keys(n.kids), func(a, b string) int { return strings.Compare(key(a), key(b)) })
	var body bytes.Buffer
	for _, name := range names {
		kid := n.kids[name]
		if kid.kids != nil {
			kid.sum = kid.tree()
		}
		_, _ = fmt.Fprintf(&body, "%s %s\x00", kid.mode, name)
		body.Write(kid.sum)
	}
	sum := sha1.New() //nolint:gosec // git's object name, as above
	_, _ = fmt.Fprintf(sum, "tree %d\x00", body.Len())
	_, _ = body.WriteTo(sum)
	return sum.Sum(nil)
}
