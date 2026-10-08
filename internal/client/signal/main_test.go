package signal_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (README "Testing"). The tests run in testing/synctest
// bubbles, which also fail a test whose goroutines are still blocked when it returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
