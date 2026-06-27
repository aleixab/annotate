package diff

import "testing"

// makeDiff builds a minimal unified diff body for tests.
func TestMapLine_LinesAddedAbove(t *testing.T) {
	// Two lines inserted before the anchor; everything below shifts down by 2.
	d := `@@ -1,3 +1,5 @@
 a
+x
+y
 b
 c
`
	h := Parse(d)
	// old line 2 ("b") -> new line 4.
	got, deleted := MapLine(h, 2)
	if deleted || got != 4 {
		t.Fatalf("want 4,false got %d,%v", got, deleted)
	}
	// old line 3 ("c") -> new line 5.
	got, _ = MapLine(h, 3)
	if got != 5 {
		t.Fatalf("want 5 got %d", got)
	}
}

func TestMapLine_LinesRemovedAbove(t *testing.T) {
	// Two lines removed before the anchor; lines below shift up by 2.
	d := `@@ -1,5 +1,3 @@
 a
-x
-y
 b
 c
`
	h := Parse(d)
	// old line 4 ("b") -> new line 2.
	got, deleted := MapLine(h, 4)
	if deleted || got != 2 {
		t.Fatalf("want 2,false got %d,%v", got, deleted)
	}
}

func TestMapLine_DeletedAtAnchor(t *testing.T) {
	d := `@@ -1,3 +1,2 @@
 a
-b
 c
`
	h := Parse(d)
	// old line 2 ("b") was deleted.
	_, deleted := MapLine(h, 2)
	if !deleted {
		t.Fatalf("expected line 2 to be reported deleted")
	}
}

func TestMapLine_ChangeBelowAnchor(t *testing.T) {
	// Edit happens after the anchor; the anchor line is unaffected.
	d := `@@ -8,3 +8,4 @@
 h
+new
 i
 j
`
	h := Parse(d)
	// old line 2 is well before the hunk -> unchanged.
	got, deleted := MapLine(h, 2)
	if deleted || got != 2 {
		t.Fatalf("want 2,false got %d,%v", got, deleted)
	}
}

func TestMapLine_MultipleHunks(t *testing.T) {
	d := `@@ -1,2 +1,3 @@
 a
+x
 b
@@ -10,2 +11,1 @@
 j
-k
`
	h := Parse(d)
	// Line 5 sits between hunks: shifted by +1 from the first hunk.
	got, _ := MapLine(h, 5)
	if got != 6 {
		t.Fatalf("want 6 got %d", got)
	}
	// Line 10 ("j") is context in second hunk -> 11.
	got, _ = MapLine(h, 10)
	if got != 11 {
		t.Fatalf("want 11 got %d", got)
	}
}
