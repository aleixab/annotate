// Package diff parses unified git diffs into hunks and walks a single line
// number from the old (baseline) side of the diff to its position on the new
// (working-tree) side. This is the core of remapping a note across edits made
// elsewhere in the file.
package diff

import (
	"regexp"
	"strconv"
	"strings"
)

// kind classifies a line within a hunk body.
type kind int

const (
	context kind = iota
	del
	add
)

type line struct {
	kind kind
}

// Hunk is one @@ ... @@ block of a unified diff.
type Hunk struct {
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	lines    []line
}

var headerRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// Parse turns unified diff text into an ordered list of hunks.
func Parse(out string) []Hunk {
	var hunks []Hunk
	cur := -1
	for _, ln := range strings.Split(out, "\n") {
		if m := headerRe.FindStringSubmatch(ln); m != nil {
			hunks = append(hunks, Hunk{
				OldStart: atoiDefault(m[1], 1),
				OldLines: atoiDefault(m[2], 1),
				NewStart: atoiDefault(m[3], 1),
				NewLines: atoiDefault(m[4], 1),
			})
			cur = len(hunks) - 1
			continue
		}
		if cur < 0 || ln == "" {
			continue
		}
		switch ln[0] {
		case ' ':
			hunks[cur].lines = append(hunks[cur].lines, line{context})
		case '+':
			hunks[cur].lines = append(hunks[cur].lines, line{add})
		case '-':
			hunks[cur].lines = append(hunks[cur].lines, line{del})
		case '\\':
			// "\ No newline at end of file" — ignore.
		default:
			// Any other prefix means we've left the hunk body.
			cur = -1
		}
	}
	return hunks
}

// MapLine walks oldLine (1-based, on the baseline side) through the hunks and
// returns its position on the new side. deleted is true if the line itself was
// removed, in which case newLine is the new-side position where it used to be.
func MapLine(hunks []Hunk, oldLine int) (newLine int, deleted bool) {
	offset := 0
	for _, h := range hunks {
		if oldLine < h.OldStart {
			return oldLine + offset, false
		}
		if oldLine < h.OldStart+h.OldLines {
			oldCur := h.OldStart
			newCur := h.NewStart
			for _, dl := range h.lines {
				switch dl.kind {
				case context:
					if oldCur == oldLine {
						return newCur, false
					}
					oldCur++
					newCur++
				case del:
					if oldCur == oldLine {
						return newCur, true
					}
					oldCur++
				case add:
					newCur++
				}
			}
		}
		offset += h.NewLines - h.OldLines
	}
	return oldLine + offset, false
}
