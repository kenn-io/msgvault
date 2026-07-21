package main

import (
	"fmt"
	"os"

	"go.kenn.io/msgvault/internal/codegenfix"
)

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: codegenfix <generated-types.go>")
		os.Exit(2)
	}
	path := os.Args[1]
	// #nosec G703 -- this local build tool intentionally rewrites its caller-selected generated file.
	source, err := os.ReadFile(path)
	if err == nil {
		source, err = codegenfix.RewriteGeneratedValidators(source)
	}
	if err == nil {
		// #nosec G703 -- path is the same explicit generated-file argument read above.
		err = os.WriteFile(path, source, 0o600)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
