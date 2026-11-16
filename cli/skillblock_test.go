package cli

import (
	"fmt"
	"strings"
	"testing"
)

// shippedBlock is the block install will write.
func shippedBlock(t *testing.T) string {
	t.Helper()
	return skillBlock(skillBody)
}

// The stamp is pinned to literal hex, not recomputed on both sides of the comparison. What it
// covers is the contract: fold the front matter in, add a trailing newline, or shorten the digest,
// and it is red here rather than every block already installed across a fleet reading as
// hand-edited at its next install. The last entry is today's body, so an edit to skill/canyon.md
// moves it on purpose and a silent change of recipe moves it by accident.
func TestTheStampIsTwelveHexOfKnownBytes(t *testing.T) {
	for in, want := range map[string]string{
		"":           "e3b0c44298fc",
		"# Canyon":   "bdf920fb9258",
		"# Canyon\n": "33facedb34a7",
		skillBody:    "431085511d3b",
	} {
		if got := stamp(in); got != want || len(got) != stampChars {
			t.Errorf("stamp(%.12q…) = %q, want %q", in, got, want)
		}
	}
}

// And it covers the body between the markers, with the front matter outside it: the front matter is
// per target, so a stamp that covered it would make the same text read differently in each harness.
// The marker in --print's own output is held to today's literal, so the recipe cannot drift on the
// writing side while stamp() alone stays green.
func TestTheStampCoversTheBodyAndNotTheFrontMatter(t *testing.T) {
	for _, harness := range []string{targetClaude, targetCursor, targetAgents} {
		text, err := skillText(harness)
		if err != nil {
			t.Fatal(err)
		}
		ref, found, err := findBlock("SKILL.md", text)
		if err != nil || !found {
			t.Fatalf("%s: the block Canyon just wrote was not found: found %v, err %v", harness, found, err)
		}
		if ref.Body != skillBody {
			t.Errorf("%s: the body between the markers is not the shipped text:\n%q", harness, ref.Body)
		}
		if ref.Hash != "431085511d3b" {
			t.Errorf("%s: the marker carries %q; today's body stamps to 431085511d3b", harness, ref.Hash)
		}
		if ref.Version != blockVersion {
			t.Errorf("%s: the marker carries version %q, want %q", harness, ref.Version, blockVersion)
		}
	}
	for _, marker := range []string{blockOpen, blockEnd} {
		if strings.Contains(skillDescription+skillBody, marker) {
			t.Errorf("the shipped text carries %q, which is the marker findBlock looks for", marker)
		}
	}
}

// A block a later release wrote is found and read, never rewritten blind: this binary has to be
// able to see a version it does not write before it decides what to do about it.
func TestAMarkerALaterReleaseWroteIsStillFoundAndRead(t *testing.T) {
	later := "<!-- canyon:skill v:2 hash:0123456789ab scope:org -->\nnewer body\n" + blockEnd + "\n"
	ref, found, err := findBlock("AGENTS.md", "a person's words\n\n"+later)
	switch {
	case err != nil || !found:
		t.Fatalf("a v:2 block was not found: found %v, err %v", found, err)
	case ref.Version != "2":
		t.Errorf("version read as %q, want \"2\"", ref.Version)
	case ref.Hash != "0123456789ab":
		t.Errorf("stamp read as %q, want \"0123456789ab\"", ref.Hash)
	case ref.Body != "newer body\n":
		t.Errorf("body read as %q", ref.Body)
	}
}

// A marker is recognised at the start of a line and nowhere else. A file that quotes one in a
// sentence — or a policy name that carries one — is still a file Canyon can write into, and a body
// that would carry one onto a line of its own is refused before anything is written.
func TestAQuotedMarkerIsTextAndAMarkerInTheBodyIsRefused(t *testing.T) {
	block := shippedBlock(t)
	quoted := "Canyon owns the lines between `" + blockOpen + "1 ... -->` and `" + blockEnd + "`.\n\n"
	whole := quoted + block + "\ntheir own words below\n"
	ref, found, err := findBlock("AGENTS.md", whole)
	switch {
	case err != nil || !found:
		t.Fatalf("a file quoting the markers was refused: found %v, err %v", found, err)
	case ref.Start != len(quoted):
		t.Errorf("the block starts at %d, and the quoted line ends at %d", ref.Start, len(quoted))
	case whole[ref.Start:ref.End] != block:
		t.Errorf("the range is not the block:\n%q", whole[ref.Start:ref.End])
	}
	if _, found, err := findBlock("AGENTS.md", "see "+blockEnd+" above\n"); found || err != nil {
		t.Errorf("a closer mid-line: found %v, err %v", found, err)
	}
}

// A marker inside a fenced example is an example. A file that documents the marker format — this
// repo's own AGENTS.md is one edit away from being that file — must not have its example found,
// refused as malformed, or one day written over.
func TestAMarkerInsideAFencedExampleIsNotABlock(t *testing.T) {
	block := shippedBlock(t)
	documented := "# How Canyon marks its block\n\n```markdown\n" + block + "```\n\nand that is all.\n"
	if _, found, err := findBlock("AGENTS.md", documented); found || err != nil {
		t.Errorf("a marker pair inside a fence: found %v, err %v", found, err)
	}
	if _, found, err := findBlock("AGENTS.md", "~~~\n"+blockBegin+"0123456789ab -->\n~~~\n"); found || err != nil {
		t.Errorf("an unpaired opener inside a tilde fence: found %v, err %v", found, err)
	}
	// And the real block below the example is still the one Canyon owns.
	whole := documented + "\n" + block
	ref, found, err := findBlock("AGENTS.md", whole)
	if err != nil || !found || whole[ref.Start:ref.End] != block {
		t.Fatalf("the live block below the example was not found: found %v, err %v", found, err)
	}
	if ref.Start != len(documented)+1 {
		t.Errorf("the block starts at %d, and the example ends at %d", ref.Start, len(documented)+1)
	}
}

// A malformed pair is refused by line and with the edit that clears it, never treated as "no block
// here" — the caller's answer to that is to append, which writes a second Canyon block into a
// person's file.
func TestAMalformedMarkerPairIsRefusedByLineRatherThanAppendedTo(t *testing.T) {
	block := shippedBlock(t)
	whole := "# A person's own file\n\n" + block + "\ntheir own words below it\n"
	ref, found, err := findBlock("AGENTS.md", whole)
	if err != nil || !found || whole[ref.Start:ref.End] != block {
		t.Fatalf("a well-formed block was not returned whole: found %v, err %v", found, err)
	}
	if _, found, err := findBlock("AGENTS.md", "nothing of ours here\n"); found || err != nil {
		t.Errorf("a file with no block: found %v, err %v", found, err)
	}
	second := fmt.Sprintf("AGENTS.md:1 and AGENTS.md:%d", strings.Count(block, "\n")+1)
	for name, c := range map[string]struct{ file, at string }{
		"two openers":                             {block + block, second},
		"an opener, no closer":                    {blockBegin + "0123456789ab -->\n# Canyon\n", "AGENTS.md:1"},
		"a closer first":                          {blockEnd + "\n" + block, "AGENTS.md:1"},
		"a closer alone":                          {"their own words\n" + blockEnd + "\n", "AGENTS.md:2"},
		"a second closer":                         {block + "more\n" + blockEnd + "\n", fmt.Sprintf("AGENTS.md:%d closes", strings.Count(block, "\n")+2)},
		"a marker wrapped over two lines":         {"<!-- canyon:skill v:1\nhash:0123456789ab -->\n# Canyon\n" + blockEnd + "\n", "AGENTS.md:1 opens a Canyon block with a marker"},
		"a stamp that is not hex":                 {blockBegin + "zzNOTHEX--ab -->\n# Canyon\n" + blockEnd + "\n", "AGENTS.md:1 opens a Canyon block with a marker"},
		"a fence nothing closes, above the block": {"```\n" + block, "AGENTS.md:1 opens a fenced"},
	} {
		_, found, err := findBlock("AGENTS.md", c.file)
		switch {
		case err == nil || found:
			t.Errorf("%s was accepted rather than refused: found %v, err %v", name, found, err)
		case !strings.Contains(err.Error(), c.at):
			t.Errorf("%s: the refusal does not name %s: %v", name, c.at, err)
		case !strings.Contains(err.Error(), "run this again"):
			t.Errorf("%s: the refusal names no remedy: %v", name, err)
		}
	}
}

// A CRLF checkout and a trailing space on the closer are still the markers: the closer is matched
// with its line ending and trailing blanks trimmed, exactly as the opener always was.
func TestAClosersLineEndingOrTrailingSpaceDoesNotHideTheBlock(t *testing.T) {
	block := shippedBlock(t)
	for name, file := range map[string]string{
		"CRLF":                     strings.ReplaceAll(block, "\n", "\r\n"),
		"a space after the closer": strings.Replace(block, blockEnd+"\n", blockEnd+" \n", 1),
		"a byte order mark":        "\ufeff" + block,
	} {
		if ref, found, err := findBlock("AGENTS.md", file); !found || err != nil || ref.Hash != stamp(skillBody) {
			t.Errorf("%s: found %v, err %v, hash %q", name, found, err, ref.Hash)
		}
	}
}

// The marker vocabulary as bytes, against a literal the code does not share: every other test
// derives its expectation from the constants, so a typo in one would stay self-consistent.
func TestTheMarkerBytesAreExactlyThese(t *testing.T) {
	if blockOpen != "<!-- canyon:skill v:" || blockEnd != "<!-- /canyon:skill -->" || blockBegin != "<!-- canyon:skill v:1 hash:" {
		t.Errorf("markers: %q %q %q", blockOpen, blockBegin, blockEnd)
	}
}
