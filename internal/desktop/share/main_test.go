package share

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (the shares' watchers, the stops Start makes).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
