package session

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/MoonWX/isshoni/internal/client/credstore"
)

// TestMain fails the run when a goroutine outlives the tests (Run, a core client's goroutines).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func memStore() credstore.Store { return credstore.NewMemory() }
