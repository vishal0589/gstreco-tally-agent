package tally

import (
	"os"
	"path/filepath"
	"testing"
)

// readTestdata reads a file from ./testdata. Kept under a different name
// from A2a's parser_v3_test.go helper so the two PRs can be reviewed and
// merged in either order without a "duplicate mustFixture" conflict; once
// both land, a follow-up can consolidate.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return b
}
