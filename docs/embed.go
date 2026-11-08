// Package docs embeds published guides that the CLI prints.
package docs

import _ "embed"

//go:embed guides/agent-quickstart.md
var AgentQuickstart string
