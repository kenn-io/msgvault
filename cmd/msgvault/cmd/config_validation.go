package cmd

import (
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
)

// configSections describes local configuration consumed by an invocation.
// HTTP clients use the daemon's provider policy; serve and daemon startup
// validate every section before they can serve any archive operations.
func configSections(cmd *cobra.Command, cfg *config.Config) []config.Section {
	top := cmd
	for top.Parent() != nil && top.Parent() != cmd.Root() {
		top = top.Parent()
	}
	name := top.Name()
	if (name == "serve" && cmd.Name() != statusValue && cmd.Name() != "stop") ||
		(name == "daemon" && (cmd.Name() == "start" || cmd.Name() == "restart")) {
		return []config.Section{config.SectionAll}
	}
	var sections []config.Section
	local, _ := cmd.Flags().GetBool("local")
	localOnly := (name == "daemon" || name == "serve") && (cmd.Name() == statusValue || cmd.Name() == "stop")
	switch name {
	case "eval", "fake-vault":
		localOnly = true
	case "multimodal":
		localOnly = cmd.Name() == "probe"
	case "documents":
		localOnly = cmd.Name() == "probe-mistral"
	case "setup":
		localOnly = cmd.Name() == statusValue || cmd.Name() == "providers"
	case "person":
		localOnly = cmd.Parent() != nil && cmd.Parent().Name() == "provider" && (cmd.Name() == "login" || cmd.Name() == "models")
	case "muesli-hook":
		install, _ := cmd.Flags().GetString("install")
		localOnly = install != ""
	case "backup":
		localOnly = cmd.Name() != "create"
	case "carddav":
		localOnly = cmd.Name() == "authorize-google" || cmd.Name() == "authorize-microsoft"
	}
	if (cfg.Remote.URL != "" && !local && !localOnly) || name == "export-token" {
		sections = append(sections, "remote")
	}
	// These clients forward the operation before reading provider policy.
	// The daemon worker validates the policy from its own configuration.
	if !isDaemonConsoleSubprocess() {
		switch name {
		case "activity", "build-cache", "repair-encoding", "repair-dates", "remove-account", "import-imessage", "purge-excluded-media", "import-slackdump", "verify", "sync-calendar", "calendar", "add-slack", "add-discord", "add-teams",
			"add-omi", "add-granola", "add-twenty", "add-twilio", "add-chatwoot", "add-notion-meetings",
			"sync-beeper", "sync-slack", "sync-discord", "sync-teams", "sync-chatwoot", "sync-omi", "sync-granola", "sync-circleback", "sync-plaud", "sync-twenty", "sync-twilio", "sync-notion-meetings",
			"backfill-beeper-media", "backfill-slack-media", "backfill-discord-media", "backfill-teams-media":
			return sections
		case "add-beeper":
			if cfg.Remote.URL != "" && !local {
				return sections
			}
		case "sync-muesli", "add-muesli":
			if cfg.Remote.URL == "" || local {
				return sections
			}
		case "backup":
			if cmd.Name() == "create" {
				return sections
			}
		}
	}
	switch name {
	case "backup":
		sections = append(sections, "backup")
	case "embed", "build-embeddings", "embeddings":
		if cmd.Name() != statusValue && (isDaemonCLISubprocess() || cmd.Name() == embeddingsOptimizeWorkerName) {
			sections = append(sections, "vector")
		}
	case "eval":
		modes, _ := cmd.Flags().GetString("modes")
		rerank, _ := cmd.Flags().GetString("rerank-jev")
		needVector := false
		for mode := range strings.SplitSeq(modes, ",") {
			if mode = strings.TrimSpace(mode); mode == "vector" || mode == "hybrid" {
				needVector = true
			}
		}
		if needVector || rerank != "" {
			sections = append(sections, "vector")
		}
	case "multimodal":
		if cmd.Name() == "probe" {
			sections = append(sections, "vector")
		}
	case "documents":
		capabilities, _ := cmd.Flags().GetString("capabilities")
		if cmd.Name() == "search" || (!isDaemonCLISubprocess() &&
			cmd.Name() != "probe-mistral" && (cmd.Name() != statusValue || capabilities == "")) {
			break
		}
		if cmd.Name() == "retire" || cmd.Name() == "purge-derived" ||
			(cmd.Parent().Name() == documentVectorsSubcommand && cmd.Name() == "retry") {
			break
		}
		sections = append(sections, "attachments.documents")
		if cmd.Parent().Name() == documentVectorsSubcommand && cfg.Attachments.Documents.Index.Embeddings.Enabled {
			sections = append(sections, "vector")
		}
	case "activity":
		sections = append(sections, "activity")
	case "build-cache", "repair-encoding", "repair-dates", "remove-account", "import-imessage":
		sections = append(sections, "analytics")
		if name == "repair-encoding" && cfg.Vector.Enabled {
			sections = append(sections, "vector")
		}
	case "export-token":
		sections = append(sections, "oauth")
	case "repair-message":
		audit, _ := cmd.Flags().GetBool("audit")
		if !audit && (isDaemonCLISubprocess() || cfg.Remote.URL == "" || local) {
			sections = append(sections, "oauth")
		}
	case "add-account", "verify", "add-calendar", "sync-calendar", "calendar", "add-synctech-sms-drive":
		headless, _ := cmd.Flags().GetBool("headless")
		if !isDaemonCLISubprocess() && headless && (name == "add-account" || name == "add-calendar") {
			break
		}
		if isDaemonCLISubprocess() || cfg.Remote.URL == "" || local {
			sections = append(sections, "oauth")
		}
	case "muesli-hook":
		install, _ := cmd.Flags().GetString("install")
		if install == "" {
			sections = append(sections, "muesli")
		}
	case "carddav":
		if cmd.Name() == "authorize-google" {
			sections = append(sections, "oauth")
		}
	case "person":
		for c := cmd; c != top; c = c.Parent() {
			switch c.Name() {
			case "provider":
				if !isDaemonCLISubprocess() {
					switch cmd.Name() {
					case statusValue, "list", "consent", "reverify", "revoke", "history", "check":
						return sections
					}
				}
				all, _ := cmd.Flags().GetBool("all")
				if all && (cmd.Name() == statusValue || cmd.Name() == "revoke") {
					break
				}
				semantic, _ := cmd.Flags().GetBool("semantic-embeddings")
				if semantic {
					sections = append(sections, "vector")
				} else {
					sections = append(sections, "people.sweep")
				}
			case "sweep":
				if cmd.Name() == "run" || (isDaemonCLISubprocess() && cmd.Name() == statusValue) {
					sections = append(sections, "people.sweep")
				}
			case "enrichment":
				if cmd.Name() == "run" || cmd.Name() == "suppress" || (isDaemonCLISubprocess() && cmd.Name() == statusValue) {
					sections = append(sections, "people.enrichment")
				}
			}
		}
	case "setup":
		if cmd.Name() == "providers" || cmd.Name() == statusValue {
			sections = append(sections, setupConfigSections()...)
		}
	case "purge-excluded-media":
		sections = append(sections, "beeper", "slack", "discord", "teams", "chatwoot")
	}
	// Source-specific workers share the same configuration rules as their
	// direct commands. Validate one provider, rather than every source policy.
	for _, prefix := range []string{"add-", "sync-", "import-", "backfill-"} {
		if source, ok := strings.CutPrefix(name, prefix); ok {
			source = strings.TrimSuffix(source, "-media")
			if name == "add-teams" {
				continue
			}
			switch source {
			case "beeper", "slack", "discord", "teams", "chatwoot", "omi", "granola", "circleback", "plaud", "muesli", "twenty", "twilio", "fastmail":
				sections = append(sections, config.Section(source))
			case "notion-meetings":
				sections = append(sections, "notion_meetings")
			case "slackdump":
				sections = append(sections, "slack")
			}
		}
	}
	return sections
}

func setupConfigSections() []config.Section {
	return []config.Section{"vector", "attachments.documents", "people.sweep", "activity", "beeper", "slack", "discord", "teams"}
}

func commandRuntimeOverrides(cmd *cobra.Command) config.RuntimeOverrides {
	overrides := serveRuntimeOverrides(cmd)
	if cmd.Name() == "export-token" && cmd.Flags().Changed("allow-insecure") {
		value, _ := cmd.Flags().GetBool("allow-insecure")
		overrides.RemoteAllowInsecure = &value
	}
	return overrides
}
