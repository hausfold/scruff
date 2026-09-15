package commands

import (
	"reflect"
	"strings"
	"testing"
)

// The hunk index and the range comparison are pure functions over git's diff
// stream, and the acceptance suite drives the whole verb on real trees. These
// pin the parsing traps on their own, where a wrong range is a wrong number
// rather than a wrong table.

func diffOf(lines ...string) string { return strings.Join(lines, "\n") }

func TestHunkSpansReportTheBaseSide(t *testing.T) {
	// -a,b is the ancestor's numbering; +c,d is the lane's own, and two lanes'
	// own numbers drift apart with every insertion above the hunk.
	got := hunkSpans(diffOf("diff --git a/f b/f", "--- a/f", "+++ b/f", "@@ -10,4 +14,9 @@"))
	want := []span{{"f", 10, 13}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHunkSpansCountlessHeaderIsOneLine(t *testing.T) {
	got := hunkSpans(diffOf("diff --git a/f b/f", "--- a/f", "+++ b/f", "@@ -10 +10 @@"))
	if want := []span{{"f", 10, 10}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHunkSpansPureInsertionClaimsTheSeam(t *testing.T) {
	// "-7,0" touches nothing of the base, but two lanes inserting there still
	// collide — so it claims the join between 7 and 8 rather than nothing.
	got := hunkSpans(diffOf("diff --git a/f b/f", "--- a/f", "+++ b/f", "@@ -7,0 +8,3 @@"))
	if want := []span{{"f", 7, 8}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHunkSpansAddedAndDeletedFilesClaimEverything(t *testing.T) {
	added := hunkSpans(diffOf("diff --git a/f b/f", "--- /dev/null", "+++ b/f", "@@ -0,0 +1,3 @@"))
	deleted := hunkSpans(diffOf("diff --git a/f b/f", "--- a/f", "+++ /dev/null", "@@ -1,3 +0,0 @@"))
	want := []span{{"f", 0, overlapWhole}}
	if !reflect.DeepEqual(added, want) || !reflect.DeepEqual(deleted, want) {
		t.Fatalf("added %v, deleted %v, want %v", added, deleted, want)
	}
}

func TestHunkSpansKeepAPathWithASpace(t *testing.T) {
	// git appends a tab after a name that holds a space; the tab is not part
	// of the name, and the space is.
	got := hunkSpans(diffOf("diff --git a/my file.md b/my file.md", "--- a/my file.md\t", "+++ b/my file.md\t", "@@ -4,2 +4,2 @@"))
	if want := []span{{"my file.md", 4, 5}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHunkSpansDoNotReadContentAsAHeader(t *testing.T) {
	// A line whose own text starts with "++ " arrives as "+++ ", and reading it
	// as a header files every LATER hunk under a garbage path — the file drops
	// out of the comparison silently, which is the worst way to fail. A removed
	// "-- " line is the same trap on the other side.
	for _, content := range []string{"+++ still content", "--- /dev/null"} {
		got := hunkSpans(diffOf("diff --git a/f b/f", "--- a/f", "+++ b/f", "@@ -2,2 +2,2 @@", content, "@@ -10,1 +10,1 @@"))
		if want := []span{{"f", 2, 3}, {"f", 10, 10}}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: got %v, want %v", content, got, want)
		}
	}
}

func TestCompareWithinTheFuzzIsTheSameRegion(t *testing.T) {
	got := compare([]span{{"doc.md", 10, 10}}, []span{{"doc.md", 12, 12}})
	if want := []finding{{"doc.md", true, 10, 12}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got[0].detail() != "L10-12" {
		t.Fatalf("detail %q", got[0].detail())
	}
}

func TestCompareBeyondTheFuzzIsTheSameFileOnly(t *testing.T) {
	// The whole point: co-editing a long shared file is normal and must not cry
	// wolf. A warning you learn to ignore is worse than no warning.
	got := compare([]span{{"doc.md", 10, 10}}, []span{{"doc.md", 50, 50}})
	if want := []finding{{path: "doc.md"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCompareSaysNothingAcrossFilesAndForAnEmptySide(t *testing.T) {
	if got := compare([]span{{"a.md", 1, 1}}, []span{{"b.md", 1, 1}}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	// "This lane has changed nothing yet" is an ordinary state, not an edge
	// case — and it must not read the other side as overlapping itself.
	if got := compare(nil, []span{{"doc.md", 10, 10}}); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestCompareLoudSortsAboveQuiet(t *testing.T) {
	got := compare(
		[]span{{"a.md", 1, 1}, {"b.md", 1, 1}, {"c.md", 1, 1}},
		[]span{{"a.md", 40, 40}, {"b.md", 2, 2}, {"c.md", 0, overlapWhole}},
	)
	if len(got) != 3 || !got[0].hunk || !got[1].hunk || got[2].hunk {
		t.Fatalf("got %v", got)
	}
	if got[0].path != "b.md" || got[1].path != "c.md" || got[1].detail() != "the whole file" {
		t.Fatalf("got %v", got)
	}
}
