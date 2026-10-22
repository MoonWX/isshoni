package app

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (03 §22.1's app row): the app starts managers, the
// probe and its parts in goroutines of its own.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
