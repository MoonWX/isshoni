package settings

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: the debounce runs on a timer's goroutine.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
