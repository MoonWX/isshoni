package native

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: every Engine's log drain must end with Close.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
