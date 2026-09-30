// Command mixedapp links example.com/mixed, an MIT module with a GPL-3.0 LICENSE-GPL file: the license gate must
// fail on it, although its primary LICENSE is allowlisted.
package main

import (
	"fmt"

	"example.com/mixed"
)

func main() {
	fmt.Println(mixed.Name)
}
