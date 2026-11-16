package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The block Canyon owns inside a file it shares with a person. HTML comments render as nothing in
// Markdown, in a Cursor .mdc and in AGENTS.md. `v:` is the block format's version, not the
// binary's: the finder matches the prefix, so a block written by any version is found and read;
// refusing to rewrite one a later release wrote is install's check, on the Version it returns.
const (
	blockOpen  = "<!-- canyon:skill v:"
	blockBegin = blockOpen + blockVersion + " hash:"
	blockEnd   = "<!-- /canyon:skill -->"

	blockVersion = "1"
	stampChars   = 12
)

// skillBlockRef is one Canyon block as found in a file: where its bytes are, what its opening
// marker says about them, and the body the stamp covers. install replaces Start:End and leaves
// every other byte alone; Hash against stamp(Body) tells Canyon's own last write from a person's
// edit, and Version is read so a block a later release wrote is never rewritten.
type skillBlockRef struct {
	Start, End    int
	Version, Hash string
	Body          string
}

// stamp is the first 12 hex of SHA-256 over the block's body — the bytes between the marker lines,
// the front matter excluded. It travels inside the file, which is the point: two of the three
// targets are committed and cloned, and a stamp kept anywhere else is already wrong by the time a
// teammate checks the repo out. A change detector, not a signature.
func stamp(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])[:stampChars]
}

// markerLine reports what one line of a file is, its newline stripped. A marker is recognised at
// the start of a line and nowhere else, so a file that quotes one in a sentence — or names it in a
// policy — stays a file Canyon can write into.
func markerLine(line string) (opener, closer bool) {
	line = strings.TrimRight(line, " \t\r\n") // a CRLF checkout or a trailing space is still the marker
	return strings.HasPrefix(line, blockOpen), line == blockEnd
}

// readMarker reads the tokens out of an opening marker line. The keys are read rather than the line
// matched whole, so a marker a later release wrote — a longer version, a key this binary has never
// heard of — still yields the two values every caller needs.
func readMarker(line string) (version, hash string) {
	fields := strings.TrimSuffix(strings.TrimPrefix(strings.TrimRight(line, " \t\r\n"), "<!--"), "-->")
	for _, f := range strings.Fields(fields) {
		switch key, value, _ := strings.Cut(f, ":"); key {
		case "v":
			version = value
		case "hash":
			hash = value
		}
	}
	return version, hash
}

// wholeMarker reports whether an opening marker is complete on its line and carries a stamp this
// build can read: an editor that wrapped the marker, or a hash that is not twelve hex characters,
// is refused by line rather than read as a block with no stamp and a body that starts mid-marker.
func wholeMarker(line string, version, hash string) bool {
	if !strings.HasSuffix(strings.TrimRight(line, " \t\r\n"), "-->") || version == "" || len(hash) != stampChars {
		return false
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// skillBlock is the marked block whole: the opening marker carrying the stamp, the body, the
// closing marker. The body is the embedded text, which a test holds marker-free.
func skillBlock(body string) string {
	return blockBegin + stamp(body) + " -->\n" + body + blockEnd + "\n"
}

// findBlock returns the Canyon block in file, markers included, and false when the file carries
// none. An opener counts at the start of a line and outside a fenced code block, where it is an
// example. The edges of a block are never guessed: an opener nothing closes, a closer nothing opens
// two openers, two closers and a block hidden under a fence nothing closes are each refused by
// line number with the edit that clears it, because the alternative is writing over a person's
// text or appending a second copy of ours.
func findBlock(path, file string) (skillBlockRef, bool, error) {
	var ref skillBlockRef
	openAt, bodyAt, closeAt := -1, 0, -1
	openLine, closeLine, secondOpen, secondClose, fenceLine, badOpen := 0, 0, 0, 0, 0, 0
	off := 0
	body := file
	if strings.HasPrefix(file, "\ufeff") { // a byte order mark is not the first byte of a marker
		body, off = file[len("\ufeff"):], len("\ufeff")
	}
	var fence fenceState
	for i, line := range strings.SplitAfter(body, "\n") {
		if fence.inside(line) {
			if fenceLine == 0 || fence.open == 0 {
				fenceLine = i + 1 // the line that opened the fence we are in, or just left
			}
			off += len(line)
			continue
		}
		switch opener, closer := markerLine(line); {
		case opener && openAt < 0:
			openAt, bodyAt, openLine = off, off+len(line), i+1
			if ref.Version, ref.Hash = readMarker(line); !wholeMarker(line, ref.Version, ref.Hash) {
				badOpen = i + 1
			}
		case opener:
			secondOpen = i + 1
		case closer && closeAt < 0:
			closeAt, closeLine, ref.End = off, i+1, off+len(line)
		case closer:
			secondClose = i + 1
		}
		off += len(line)
	}
	switch {
	case badOpen > 0:
		return skillBlockRef{}, false, fmt.Errorf(
			"%s:%d opens a Canyon block with a marker that is not whole on its line or carries no stamp this build can read. Restore the marker, or delete the block, markers included, and run this again",
			path, badOpen)
	case openAt < 0 && closeAt < 0 && fence.open > 0:
		return skillBlockRef{}, false, fmt.Errorf(
			"%s:%d opens a fenced code block that nothing closes, so anything below it reads as an example. Close the fence, and run this again",
			path, fenceLine)
	case openAt < 0 && closeAt < 0:
		return skillBlockRef{}, false, nil
	case closeAt >= 0 && (openAt < 0 || closeAt < openAt):
		return skillBlockRef{}, false, fmt.Errorf(
			"%s:%d closes a Canyon block that nothing above it opens. Delete that line, or put the opening marker above the block, and run this again",
			path, closeLine)
	case closeAt < 0:
		return skillBlockRef{}, false, fmt.Errorf(
			"%s:%d opens a Canyon block that nothing below it closes. Put %s on its own line at the end of the block, or delete line %d, and run this again",
			path, openLine, blockEnd, openLine)
	case secondOpen > 0:
		return skillBlockRef{}, false, fmt.Errorf(
			"%s:%d and %s:%d both open a Canyon block, and Canyon writes one. Delete one block, markers included, and run this again",
			path, openLine, path, secondOpen)
	case secondClose > 0:
		return skillBlockRef{}, false, fmt.Errorf(
			"%s:%d closes a Canyon block that %s:%d already closed. Delete that line, and run this again",
			path, secondClose, path, closeLine)
	}
	ref.Start, ref.Body = openAt, file[bodyAt:closeAt]
	return ref, true, nil
}
