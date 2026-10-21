package credstore

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (README "Testing"): the concurrency cases start some.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
