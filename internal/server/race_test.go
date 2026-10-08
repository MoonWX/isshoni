//go:build race

package server_test

// raceEnabled says whether the tests run under the race detector: TestSetupURLCommand builds cmd/isshoni the same
// way, so the build comes from the test run's cache.
const raceEnabled = true
