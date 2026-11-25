package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"time"
)

// nodeOn is whether a Node app builds on CANYON_BUILDER_NODE (contracts/executor.md, A Node app builds on Canyon's
// Node builder): set, and with no CANYON_BUILDER, which is one builder for everything.
func (e *Executor) nodeOn() bool { return e.NodeBuilder != "" && e.Builder == "" }

// nodeBuilder is the Node builder's reference as pack is handed it, and its image ID, pulled first when the daemon
// has none; a pull is when GET /builder's waited_ms says Canyon's image was made ready.
func (e *Executor) nodeBuilder(ctx context.Context) (ref, id string, err error) {
	ref = untrusted(e.NodeBuilder)
	_, absent := child(ctx, e.docker(), "image", "inspect", "--format", "{{.Id}}", ref).Output()
	if id, err = e.idOf(ctx, ref); err != nil {
		return "", "", err
	}
	if absent != nil {
		e.nodeReady.Store(time.Now().UnixNano())
	}
	return ref, id, nil
}

// nodeIdentity is GET /builder's builder with a Node builder: the SHA-256 of its image ID, a newline, and the name of
// canyon-builder's own (ownBuilder), the tag its two files' hash makes. The full builder's part is its files, never an
// image ID, which exists only once it is downloaded: a reuse check never downloads it.
func nodeIdentity(nodeID string) string {
	sum := sha256.Sum256([]byte(nodeID + "\n" + ownBuilder.repo))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// detected is the line pack prints once detection passed: what the Node builder's build writes before it is held back,
// so a tree it detects nothing in shows only canyon-builder's build.
const detected = "===> RESTORING"

// gated holds what is written to it until detected is, then writes it all on and passes the rest through.
type gated struct {
	out  io.Writer
	held bytes.Buffer
	open bool
}

func (g *gated) Write(p []byte) (int, error) {
	if g.open {
		return g.out.Write(p)
	}
	g.held.Write(p)
	if bytes.Contains(g.held.Bytes(), []byte(detected)) {
		return len(p), g.release()
	}
	return len(p), nil
}

// release writes what was held, as a build that failed after detection, or for another reason, shows it.
func (g *gated) release() error {
	g.open = true
	_, err := g.out.Write(g.held.Bytes())
	g.held.Reset()
	return err
}
