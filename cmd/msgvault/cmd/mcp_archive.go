package cmd

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func addMCPSourceFamilies(f *mcpserver.ArchiveFamilies, kind string) {
	switch kind {
	case "gcal":
		f.Calendar = true
	case "granola", "circleback", "pocket", "muesli", "notion_meetings", "meeting_import", "omi", "plaud", "twilio", "twenty", "chatwoot":
		f.Meetings = true
	}
	switch kind {
	case "gmail", "google_voice", "inline", "beeper", "slack", "slackdump", "teams", "discord", "whatsapp", "telegram", "matrix", "apple_messages", "synctech_sms", "imazing_csv", "facebook_messenger", "chatwoot", "line":
		f.Chat = true
	}
	if kind == "slack" || kind == "slackdump" {
		f.Slack = true
	}
	switch kind {
	case "gmail", "imap", "beeper", "slack", "slackdump", "teams", "discord", "line":
		f.Drafts = true
	}
}

func discoverMCPFamilies(ctx context.Context, st *daemonclient.Client, log *slog.Logger) *mcpserver.ArchiveFamilies {
	families := &mcpserver.ArchiveFamilies{}
	client, err := st.GeneratedClient()
	if err != nil {
		log.Warn("MCP archive families hidden because daemon discovery failed", "error", err)
		return families
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sources, err := client.ListSourceStatusWithResponse(probeCtx, &generated.ListSourceStatusRequestOptions{})
	cancel()
	if err != nil || sources.JSON200 == nil {
		log.Warn("MCP source families hidden because source discovery failed", "error", err)
	} else {
		for _, source := range sources.JSON200.Sources {
			addMCPSourceFamilies(families, source.SourceType)
		}
	}
	probeCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	connections, err := client.ListCardDAVConnectionsWithResponse(probeCtx)
	cancel()
	if err != nil || connections.JSON200 == nil {
		log.Warn("MCP CardDAV tools hidden because connection discovery failed", "error", err)
	} else {
		for _, connection := range connections.JSON200.Connections {
			if !connection.Orphaned {
				families.CardDAV = true
			}
		}
	}
	probeCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	settings, err := client.GetSettingsWithResponse(probeCtx)
	cancel()
	if err != nil || settings.JSON200 == nil {
		log.Warn("MCP Kata tools hidden because settings discovery failed", "error", err)
	} else {
		var enabled, endpoint bool
		for _, setting := range settings.JSON200.Settings {
			if setting.Value == nil || setting.Value.SettingValue_OneOf == nil {
				continue
			}
			value := setting.Value.SettingValue_OneOf
			switch setting.Key {
			case "integrations.kata.enabled":
				v, _ := value.AsSettingValue_OneOf_3()
				enabled = v.Boolean
			case "integrations.kata.endpoint":
				v, _ := value.AsSettingValue_OneOf_0()
				endpoint = strings.TrimSpace(v.String) != ""
			}
		}
		families.Kata = enabled && endpoint
	}
	return families
}
