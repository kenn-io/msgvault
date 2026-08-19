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

func TestBeeperSummariesReportOverCapBytesSeparately(t *testing.T) {
	tests := []struct {
		name  string
		sum   beeper.ImportSummary
		print func(*bytes.Buffer, *beeper.ImportSummary)
		want  string
	}{
		{
			name: "sync exact size",
			sum: beeper.ImportSummary{
				AttachmentsSkipped:      3,
				AttachmentsOverCap:      2,
				AttachmentsOverCapBytes: 15 << 20,
			},
			print: func(out *bytes.Buffer, sum *beeper.ImportSummary) {
				cmd := &cobra.Command{}
				cmd.SetOut(out)
				printBeeperSummary(cmd, "account", sum)
			},
			want: "2 media over size cap (15.0M total), 1 media skipped by policy",
		},
		{
			name: "backfill lower bound",
			sum: beeper.ImportSummary{
				AttachmentsSkipped:            1,
				AttachmentsOverCap:            1,
				AttachmentsOverCapBytes:       (5 << 20) + 1,
				AttachmentsOverCapUnknownSize: 1,
			},
			print: func(out *bytes.Buffer, sum *beeper.ImportSummary) {
				writeBeeperMediaBackfillSummary(out, "account", sum)
			},
			want: "1 media over size cap (at least 5.0M total)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			tt.print(&output, &tt.sum)
			assert.Contains(t, output.String(), tt.want)
		})
	}
}
