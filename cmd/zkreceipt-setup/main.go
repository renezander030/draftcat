// Command zkreceipt-setup regenerates the embedded Groth16 artifacts.
package main

import (
	"fmt"
	"os"

	"github.com/renezander030/draftcat/internal/zkreceipt"
)

func main() {
	directory := "internal/zkreceipt/artifacts"
	if len(os.Args) == 2 {
		directory = os.Args[1]
	}
	if err := zkreceipt.GenerateArtifacts(directory); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("generated Draftcat ZK approval artifacts in", directory)
}
