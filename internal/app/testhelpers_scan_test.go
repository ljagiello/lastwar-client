package app

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the test's working directory until it finds go.mod, returning the module
// root. The source-scanning lint tests below live in internal/app but must scan the whole module
// tree (the sfs/crypto/gsl packages too), not just this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking up from the test directory")
		}
		dir = parent
	}
}
