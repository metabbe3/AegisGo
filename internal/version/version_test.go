package version

import "testing"

// TestCommitDefaults marks the ldflags default so the /status build line is
// never empty ("dev" for source builds is the documented fallback).
func TestCommitDefaults(t *testing.T) {
	if Commit == "" {
		t.Fatal("Commit must never be empty — ldflags default is \"dev\"")
	}
}
