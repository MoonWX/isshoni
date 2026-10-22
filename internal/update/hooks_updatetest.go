//go:build updatetest

package update

import (
	"fmt"
	"os"
)

// The test hooks of 06 §8.11, compiled only with -tags updatetest (the updater e2e's CI builds and 03's DK12 tests;
// never a release, whose check fails a binary that contains these variable names or other build tags, 06 §5.2):
//
//   - ISSHONI_UPDATE_TEST_BASE replaces the releases URL and the beta API (testEndpoints: <base>/latest/download/…,
//     every manifest URL under <base>, the listing at <base>/api/releases); an http://127.0.0.1:<port> origin is then
//     allowed, and redirects stay under the base;
//   - ISSHONI_UPDATE_TEST_SIGNERS names an allowed-signers file used instead of the embedded keys;
//   - Eligible also accepts X.Y.Z-ci.N, and a manifest of any channel (stable, beta, ci) is accepted whatever the
//     app's channel setting.
//
// 03's --update-now flag lives in cmd/isshoni-desktop under the same tag.

const (
	envTestBase    = "ISSHONI_UPDATE_TEST_BASE"
	envTestSigners = "ISSHONI_UPDATE_TEST_SIGNERS"
)

func init() {
	updateTestBuild = true
	testHooks = applyTestEnv
}

// applyTestEnv changes New's env from the two variables.
func applyTestEnv(e *env) error {
	e.testBuild = true
	if base := os.Getenv(envTestBase); base != "" {
		ep, err := testEndpoints(base)
		if err != nil {
			return fmt.Errorf("update: %s: %w", envTestBase, err)
		}
		e.ep = ep
	}
	if file := os.Getenv(envTestSigners); file != "" {
		keys, err := readSignersFile(file)
		if err != nil {
			return fmt.Errorf("update: %s: %w", envTestSigners, err)
		}
		e.signers = func() ([]TrustedKey, error) { return keys, nil }
	}
	return nil
}
