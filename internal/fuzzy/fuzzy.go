// Package fuzzy provides the last-resort anchor matcher: when blob and diff
// remapping both fail, it slides the stored context block over the current
// buffer and returns the best-matching position above a similarity threshold.
package fuzzy

import "strings"

// Levenshtein returns the edit distance between a and b.
func Levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// Ratio returns a similarity in [0,1]: 1 for identical strings, 0 for maximally
// different. Both sides are trimmed of surrounding whitespace first.
func Ratio(a, b string) float64 {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" && b == "" {
		return 1
	}
	maxLen := len([]rune(a))
	if l := len([]rune(b)); l > maxLen {
		maxLen = l
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(Levenshtein(a, b))/float64(maxLen)
}

// BestMatch slides the block (the stored context lines) over fileLines and
// returns the 1-based line number of the anchored line (block[offset]) at the
// best-scoring window, provided its average per-line similarity meets
// threshold. ok is false when no window is good enough.
func BestMatch(fileLines, block []string, offset int, threshold float64) (lineNum int, ok bool) {
	if len(block) == 0 || len(fileLines) < len(block) {
		return 0, false
	}
	if offset < 0 || offset >= len(block) {
		offset = 0
	}
	bestScore := -1.0
	bestStart := -1
	for start := 0; start+len(block) <= len(fileLines); start++ {
		sum := 0.0
		for k := range block {
			sum += Ratio(block[k], fileLines[start+k])
		}
		score := sum / float64(len(block))
		if score > bestScore {
			bestScore = score
			bestStart = start
		}
	}
	if bestStart < 0 || bestScore < threshold {
		return 0, false
	}
	return bestStart + offset + 1, true
}
