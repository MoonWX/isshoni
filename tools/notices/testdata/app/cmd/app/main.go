// Command app stands in for cmd/isshoni: it links only modules under allowed licenses, plus example.com/mpl
// (MPL-2.0), which needs an exception, and example.com/winonly on Windows only.
package main

import (
	"fmt"

	"example.com/apache"
	"example.com/bsd"
	"example.com/mit"
	"example.com/mpl"
)

func main() {
	fmt.Println(apache.Name, bsd.Name, mit.Name, mpl.Name, platform)
}
