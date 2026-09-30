// Command gplapp links example.com/gpl, a GPL-3.0 fixture dependency: the license gate must fail on it.
package main

import (
	"fmt"

	"example.com/gpl"
	"example.com/mit"
)

func main() {
	fmt.Println(gpl.Name, mit.Name)
}
