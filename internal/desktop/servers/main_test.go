package servers

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: a probe's HTTP connections must not linger.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
