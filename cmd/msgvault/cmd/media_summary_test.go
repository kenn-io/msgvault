package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/teams"
)

func TestProviderSyncSummariesReportPolicySkips(t *testing.T) {
	tests := []struct {
		name  string
		print func(*cobra.Command)
	}{
		{
			name: "beeper",
			print: func(cmd *cobra.Command) {
				printBeeperSummary(cmd, "account", &beeper.ImportSummary{AttachmentsSkipped: 2})
			},
		},
		{
			name: "slack",
			print: func(cmd *cobra.Command) {
				printSlackSummary(cmd, "workspace", &slack.ImportSummary{AttachmentsSkipped: 2})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&output)
			tt.print(cmd)
			assert.Contains(t, output.String(), "2 media skipped by policy")
		})
	}
}

func TestRemainingProviderSummariesReportPolicySkips(t *testing.T) {
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
	}{
		{
			name: "discord sync",
			print: func(out *bytes.Buffer) {
				writeDiscordSyncSummary(out, "guild", &discord.ImportSummary{MediaSkipped: 2})
			},
		},
		{
			name: "teams sync",
			print: func(out *bytes.Buffer) {
				writeTeamsSyncSummary(out, &teams.ImportSummary{InlineImagesSkipped: 2})
			},
		},
		{
			name: "beeper backfill",
			print: func(out *bytes.Buffer) {
				writeBeeperMediaBackfillSummary(out, "account", &beeper.ImportSummary{AttachmentsSkipped: 2})
			},
		},
		{
			name: "slack backfill",
			print: func(out *bytes.Buffer) {
				writeSlackMediaBackfillSummary(out, "workspace", &slack.ImportSummary{AttachmentsSkipped: 2})
			},
		},
		{
			name: "teams backfill",
			print: func(out *bytes.Buffer) {
				writeTeamsMediaBackfillSummary(out, &teams.ImportSummary{InlineImagesSkipped: 2})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			tt.print(&output)
			assert.Contains(t, output.String(), "2")
			assert.Contains(t, strings.ToLower(output.String()), "skipped by policy")
		})
	}
}
