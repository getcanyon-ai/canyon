package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/stream"

	"github.com/getcanyon-ai/canyon/executor"
)

// What an untrusted tree may expand to, the executor package's bounds; vars so a test can lower them.
var (
	maxTree    int64 = executor.MaxTree
	maxEntries       = executor.MaxEntries
)

// checkTree copies a tar of the source from r to w, entry by entry as it arrives: directories and
// regular files under the tree, at most maxTree bytes in maxEntries entries. Anything that would
// escape the tree, anything but a file or a directory (a link, a device, a fifo), and a sparse entry
// that expands beyond what the archive carried, is refused. What it copied is noted in saw.
func checkTree(r io.Reader, w io.Writer, saw *seen) error {
	tw := tar.NewWriter(w)
	if err := copyTree(r, tw, "", nil, saw); err != nil {
		return err
	}
	return tw.Close()
}

// maxProject is the largest project.toml a source's seen keeps.
const maxProject = 1 << 20

// seen is what a build's source carried, for its failed detect to read (contracts/executor.md, But a detect
// the nginx buildpack errored on; #242): every path in it, each directory above a path too, and its root
// project.toml, unread past maxProject.
type seen struct {
	paths   map[string]bool
	project []byte
	unread  bool
}

// nginxWantsYml is executor.NginxWantsYml of the source s saw: a project.toml it did not read says no.
func (s *seen) nginxWantsYml(values map[string]string) bool {
	if s.unread {
		return false
	}
	return executor.NginxWantsYml(s.project, values, func(rel string) bool { return s.paths[rel] })
}

// copyTree is checkTree's copy into tw, each entry under prefix, leaving out an entry skip says (nil:
// none), noting what it copied in saw (nil: nothing); tw stays open for what the caller adds.
func copyTree(r io.Reader, tw *tar.Writer, prefix string, skip func(string) bool, saw *seen) error {
	cr := &countingReader{r: r}
	tr := tar.NewReader(cr)
	var total int64
	for n := 0; ; n++ {
		if n >= maxEntries {
			return fmt.Errorf("tree exceeds %d entries", maxEntries)
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader { // git archive writes one first
			continue
		}
		clean := path.Clean(strings.TrimPrefix(h.Name, "./"))
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("entry %q escapes the tree", h.Name)
		}
		out := &tar.Header{Name: prefix + clean, Mode: h.Mode&0o777 | 0o644, Typeflag: h.Typeflag, ModTime: h.ModTime}
		switch h.Typeflag {
		case tar.TypeDir:
			out.Name, out.Mode = out.Name+"/", out.Mode|0o111
		case tar.TypeReg:
			if total += h.Size; total > maxTree {
				return fmt.Errorf("tree exceeds %d bytes", maxTree)
			}
			out.Size = h.Size
		default:
			return fmt.Errorf("entry %q: only files and directories are accepted", h.Name)
		}
		if skip != nil && skip(clean) {
			if _, err := io.CopyN(io.Discard, tr, out.Size); err != nil { // read, never written
				return err
			}
		} else {
			if err := tw.WriteHeader(out); err != nil {
				return err
			}
			var body io.Reader = tr
			var project *bytes.Buffer // the root project.toml's bytes, as they pass
			if saw != nil {
				for p := clean; p != "."; p = path.Dir(p) {
					saw.paths[p] = true
				}
				if clean == "project.toml" && h.Typeflag == tar.TypeReg {
					if saw.unread = out.Size > maxProject; !saw.unread {
						project = &bytes.Buffer{}
						body = io.TeeReader(tr, project)
					}
				}
			}
			if _, err := io.CopyN(tw, body, out.Size); err != nil {
				return err
			}
			if project != nil {
				saw.project = project.Bytes()
			}
		}
		if total > cr.n+executor.SparseSlack { // written far more than the archive carried
			return fmt.Errorf("entry %q expands beyond the archive; sparse entries are refused", h.Name)
		}
	}
}

// countingReader counts the bytes the archive actually carried.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ref is s as an image of CANYON_KUBE_REGISTRY's canyon/<app>, and nothing else: parsed strictly, so a
// dotless registry never defaults to Docker Hub, and on no other host, so e.Auth reaches no one else —
// whatever a swept Build's source names.
func (e *Executor) ref(s string) (name.Reference, error) {
	r, err := name.ParseReference(s, name.StrictValidation)
	if err != nil {
		return nil, fmt.Errorf("image %q: %w (CANYON_KUBE_REGISTRY is %q)", s, err, e.Registry)
	}
	repo := r.Context()
	if app, ok := strings.CutPrefix(repo.RepositoryStr(), "canyon/"); repo.RegistryStr() != e.Registry || !ok || !validApp(app) {
		return nil, fmt.Errorf("image %q is not in %s/canyon/<app> (CANYON_KUBE_REGISTRY)", s, e.Registry)
	}
	return r, nil
}

// pushSource streams tree, checked as it passes, into a single-layer image tagged
// <registry>/canyon/<app>:src-<nonce> and answers it by digest. The image carries its build's nonce,
// so its digest is its build's alone and deleting it never takes another build's source. What the
// tree carried is answered too.
func (e *Executor) pushSource(ctx context.Context, app, run string, tree io.Reader) (name.Digest, *seen, error) {
	r, err := e.ref(e.Registry + "/canyon/" + app + ":src-" + run)
	if err != nil {
		return name.Digest{}, nil, &executor.HostFailed{Message: err.Error()}
	}
	tag := r.(name.Tag)
	pr, pw := io.Pipe()
	saw := &seen{paths: map[string]bool{}}
	checked := make(chan error, 1) // what the check said, sent before the pipe closes on it: a read that ends on it finds it here
	go func() {
		err := checkTree(tree, pw, saw)
		checked <- err
		_ = pw.CloseWithError(err)
	}()
	defer func() { _ = pr.Close() }() // a push that fails early ends the copy
	img, err := mutate.AppendLayers(empty.Image, stream.NewLayer(pr))
	if err != nil {
		return name.Digest{}, nil, &executor.HostFailed{Message: "the source image: " + err.Error()}
	}
	img = mutate.Annotations(img, map[string]string{"canyon.build": run}).(v1.Image)
	if err := remote.Write(tag, img, e.registry(ctx)...); err != nil {
		select {
		case terr := <-checked:
			if terr != nil && !errors.Is(terr, io.ErrClosedPipe) { // the tree the check refused: the package's
				return name.Digest{}, nil, fmt.Errorf("tree: %w", terr)
			}
		default: // the check still copying: the registry failed first
		}
		return name.Digest{}, nil, &executor.HostFailed{Message: "the source image " + tag.String() + ": " + err.Error()} // the registry's (#2537)
	}
	<-checked // sent before the stream the write read to its end closed: saw is whole
	sum, err := img.Digest()
	if err != nil {
		return name.Digest{}, nil, &executor.HostFailed{Message: "the source image: " + err.Error()}
	}
	return tag.Context().Digest(sum.String()), saw, nil
}

// dropImage deletes the image ref names from the registry; one already gone is a success.
func (e *Executor) dropImage(ctx context.Context, ref string) error {
	r, err := e.ref(ref)
	if err != nil {
		return err
	}
	d, ok := r.(name.Digest)
	if !ok {
		return fmt.Errorf("image %q: a source is deleted by its digest", ref)
	}
	e.dropReferrers(ctx, d) // its SBOM goes with it
	if err := remote.Delete(r, e.registry(ctx)...); err != nil && !notFound(err) {
		return fmt.Errorf("image %s: %w", ref, err)
	}
	return nil
}

func (e *Executor) registry(ctx context.Context) []remote.Option {
	auth := e.Auth
	if auth == nil {
		auth = authn.Anonymous
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuth(auth)}
}

// notFound is the registry answering 404: nothing there to delete.
func notFound(err error) bool {
	var t *transport.Error
	return errors.As(err, &t) && t.StatusCode == http.StatusNotFound
}
