package utils

import "testing"

func TestTruncateStringKeepsUTF8Valid(t *testing.T) {
	got := TruncateString("abc你好", 5)
	want := "abc"
	if got != want {
		t.Fatalf("unexpected truncated string: got %q want %q", got, want)
	}
}

func TestTruncateStringHandlesExactBoundary(t *testing.T) {
	got := TruncateString("abc你好", 6)
	want := "abc你"
	if got != want {
		t.Fatalf("unexpected truncated string on rune boundary: got %q want %q", got, want)
	}
}
