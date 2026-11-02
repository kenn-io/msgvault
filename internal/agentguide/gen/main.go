// Command gen publishes the embedded MCP guide as a documentation page.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
)

func main() {
	check := flag.Bool("check", false, "Check the published guide without writing it")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(check bool) error {
	source, err := os.ReadFile("internal/agentguide/quickstart.md")
	if err != nil {
		return err
	}
	source = bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n"))
	const destination = "docs/guides/agent-quickstart.md"
	if check {
		published, err := os.ReadFile(destination)
		if err != nil {
			return err
		}
		if !bytes.Equal(source, bytes.ReplaceAll(published, []byte("\r\n"), []byte("\n"))) {
			return fmt.Errorf("%s is stale; run make agent-guide", destination)
		}
		return nil
	}
	// #nosec G703 -- destination is a fixed repository path, never selected by guide contents.
	return os.WriteFile(destination, source, 0o600)
}
