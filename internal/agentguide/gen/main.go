// Command gen updates the tool table in the published MCP guide.
package main

import (
	"bytes"
	"cmp"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/mcp"
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
	const path = "docs/guides/agent-quickstart.md"
	source, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	generated, err := updateToolTable(source, mcp.ToolSummaries())
	if err != nil {
		return err
	}
	return syncFile(path, generated, check)
}

const (
	beginTools = "<!-- BEGIN GENERATED tools -->"
	endTools   = "<!-- END GENERATED tools -->"
)

func updateToolTable(source []byte, tools []mcp.ToolSummary) ([]byte, error) {
	text := strings.ReplaceAll(string(source), "\r\n", "\n")
	if strings.Count(text, beginTools) != 1 || strings.Count(text, endTools) != 1 {
		return nil, errors.New("guide must contain exactly one tools marker pair")
	}
	prefix, rest, _ := strings.Cut(text, beginTools)
	_, suffix, ok := strings.Cut(rest, endTools)
	if !ok {
		return nil, errors.New("tools markers are reversed")
	}
	tools = slices.Clone(tools)
	slices.SortFunc(tools, func(a, b mcp.ToolSummary) int {
		return cmp.Or(cmp.Compare(a.Security, b.Security), cmp.Compare(a.Name, b.Name))
	})
	var table strings.Builder
	table.WriteString("\n| Tool | Class | Opt-in flag | Delegated access |\n| --- | --- | --- | --- |\n")
	for _, tool := range tools {
		flag := "—"
		if tool.Flag != "" {
			flag = "`" + markdownCell(tool.Flag) + "`"
		}
		fmt.Fprintf(&table, "| `%s` | %s | %s | %s |\n", markdownCell(tool.Name), markdownCell(tool.Security), flag, markdownCell(tool.GrantPermission))
	}
	return []byte(prefix + beginTools + table.String() + endTools + suffix), nil
}

func markdownCell(value string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "|", "\\|", "`", "&#96;").Replace(value)
}

func syncFile(path string, generated []byte, check bool) error {
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if bytes.Equal(bytes.ReplaceAll(current, []byte("\r\n"), []byte("\n")), generated) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is stale; run make agent-guide", path)
	}
	// #nosec G703 -- callers supply repository paths; guide contents never select a path.
	return os.WriteFile(path, generated, 0o600)
}
