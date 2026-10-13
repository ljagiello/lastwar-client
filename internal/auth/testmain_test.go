package auth

import (
	"os"
	"testing"
)

// TestMain clears LASTWAR_STATE_DIR before any test runs. The tests isolate device state by
// pointing HOME at a temp dir, but StateFilePath prefers LASTWAR_STATE_DIR, so a shell that exports
// it (the docs tell operators to, for a device of its own) would have the tests read and overwrite
// that real device's loginKey/gameUid files.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("LASTWAR_STATE_DIR")
	os.Exit(m.Run())
}
