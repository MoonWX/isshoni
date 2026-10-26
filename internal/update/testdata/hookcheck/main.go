// Command hookcheck links internal/update the way the app does, for TestReleaseBuildHasNoTestHooks: built with the
// release's tags, its binary must not contain the updatetest hooks (06 §5.2 step 3, §8.11).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/MoonWX/isshoni/internal/update"
)

func main() {
	u, err := update.New(update.Options{DataDir: os.TempDir()})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	st, err := u.CheckNow(context.Background())
	fmt.Println(st, err, update.Eligible("0.0.1-ci.1"), update.PolicyDisabled())
}
