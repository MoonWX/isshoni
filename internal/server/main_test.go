package server_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: every server here must stop completely.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
