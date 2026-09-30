// Command badapp links a module whose license licensecheck does not recognize and one without a license file:
// the license gate must fail on both.
package main

import (
	"fmt"

	"example.com/custom"
	"example.com/nolicense"
)

func main() {
	fmt.Println(custom.Name, nolicense.Name)
}
