package fuzzy

import "testing"

func TestRatio(t *testing.T) {
	if r := Ratio("hello", "hello"); r != 1 {
		t.Fatalf("identical want 1 got %v", r)
	}
	if r := Ratio("  foo  ", "foo"); r != 1 {
		t.Fatalf("trim want 1 got %v", r)
	}
	if r := Ratio("abc", "xyz"); r != 0 {
		t.Fatalf("disjoint want 0 got %v", r)
	}
	if r := Ratio("kitten", "sitting"); r <= 0 || r >= 1 {
		t.Fatalf("partial want (0,1) got %v", r)
	}
}

func TestBestMatch_FindsShiftedBlock(t *testing.T) {
	file := []string{
		"package main",
		"",
		"// new comment added at top",
		"func helper() int {",
		"\treturn 42",
		"}",
	}
	// Block captured when the function was at the top; anchor is the return line.
	block := []string{"func helper() int {", "\treturn 42", "}"}
	offset := 1 // "return 42" is index 1 in the block
	line, ok := BestMatch(file, block, offset, 0.7)
	if !ok {
		t.Fatalf("expected a match")
	}
	if line != 5 { // "\treturn 42" is the 5th line (1-based)
		t.Fatalf("want line 5 got %d", line)
	}
}

func TestBestMatch_NoMatchBelowThreshold(t *testing.T) {
	file := []string{"totally", "different", "content", "here"}
	block := []string{"func helper() int {", "\treturn 42", "}"}
	if _, ok := BestMatch(file, block, 1, 0.7); ok {
		t.Fatalf("expected no match")
	}
}
