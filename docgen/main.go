package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra/doc"
	root "pkg.akt.dev/akashd/cmd/akash/cmd"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprint(os.Stderr, "Usage is:\n\takash_docgen <output path>\n")
		os.Exit(1)
	}
	outputPath := os.Args[1]
	cmd, _ := root.NewRootCmd()
	err := doc.GenMarkdownTree(cmd, outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed generating markdown into %q:%v\n", outputPath, err)
		os.Exit(1)
	}
}
