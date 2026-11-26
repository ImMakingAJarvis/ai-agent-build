// loopctl is the command-line client for the event-loop runtime
// (ELR-6 skeleton: only "version" exists; later cards add commands).
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "version" {
		fmt.Fprintln(os.Stderr, "usage: loopctl version")
		os.Exit(1)
	}
	fmt.Println("loopctl dev")
}
