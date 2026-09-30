package sfutest

import (
	"fmt"
	"os"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain fails the run when goroutines outlive the tests. Pion's goroutines get 2 s to settle after Close
// (02 §17); goleak's own retries stop after about 0.5 s.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		err := goleak.Find()
		for deadline := time.Now().Add(2 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			err = goleak.Find()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}
