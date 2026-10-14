package logx

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails the run when a goroutine outlives the tests: TestPionRedactsICEAgentPassword runs a real pion/ice
// Agent, whose task loop and connectivity checks must stop when the test closes it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
