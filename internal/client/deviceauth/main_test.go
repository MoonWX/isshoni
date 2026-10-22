package deviceauth_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (README "Testing"): managers poll, refresh and recheck
// in goroutines of their own. Most tests also run in testing/synctest bubbles, which fail a test whose goroutines
// are still blocked when it returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
