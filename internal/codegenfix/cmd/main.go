package main

import (
	"fmt"
	"go/format"
	"os"

	"go.kenn.io/msgvault/internal/codegenfix"
)

func main() {
	if len(os.Args) != 3 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: codegenfix <generated-types.go> <generated-client.go>")
		os.Exit(2)
	}
	if err := rewrite(os.Args[1], codegenfix.RewriteGeneratedValidators); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := rewrite(os.Args[2], codegenfix.RewriteRunQueryClient); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rewrite(path string, transform func([]byte) ([]byte, error)) error {
	// #nosec G703 -- this local build tool intentionally rewrites its caller-selected generated file.
	source, err := os.ReadFile(path)
	if err == nil {
		source, err = transform(source)
	}
	if err == nil {
		source, err = format.Source(source)
	}
	if err == nil {
		// #nosec G703 -- path is the same explicit generated-file argument read above.
		err = os.WriteFile(path, source, 0o600)
	}
	return err
}
