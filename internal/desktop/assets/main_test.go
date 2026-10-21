package assets

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests (the dev server tests run a proxy and an HTTP client).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
