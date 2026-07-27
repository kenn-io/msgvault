package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

const discordExportSchema = "msgvault-discord-export/1"

type exportDiscordOptions struct {
	Start  string
	End    string
	Format string
}

type discordExportEnvelope struct {
	Schema                string                         `json:"schema"`
	MsgvaultVersion       string                         `json:"msgvault_version"`
	SourceSyncCompletedAt time.Time                      `json:"source_sync_completed_at"`
	Guild                 discordExportGuild             `json:"guild"`
	Start                 time.Time                      `json:"start"`
	End                   time.Time                      `json:"end"`
	Containers            []store.DiscordExportContainer `json:"containers"`
}

type discordExportGuild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func newExportDiscordCmd(deps discordCommandDeps) *cobra.Command {
	cmd := newExportDiscordLocalCmd(deps)
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newExportDiscordLocalCmd(deps discordCommandDeps) *cobra.Command {
	opts := exportDiscordOptions{Format: "json"}
	cmd := &cobra.Command{
		Use:   "export-discord <guild-id-or-name>",
		Short: "Export bounded Discord history from the local archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExportDiscord(cmd, deps, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Start, "start", "", "inclusive RFC3339 lower bound")
	cmd.Flags().StringVar(&opts.End, "end", "", "exclusive RFC3339 upper bound")
	cmd.Flags().StringVar(&opts.Format, "format", "json", "output format (json)")
	return cmd
}

func runExportDiscord(
	cmd *cobra.Command,
	deps discordCommandDeps,
	selector string,
	opts exportDiscordOptions,
) error {
	start, end, err := parseDiscordExportBounds(opts.Start, opts.End)
	if err != nil {
		return usageErr(cmd, err)
	}
	if opts.Format != "json" {
		return usageErr(cmd, fmt.Errorf("unsupported --format %q (expected json)", opts.Format))
	}

	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()

	sources, err := resolveDiscordSources(st, selector)
	if err != nil {
		return usageErr(cmd, err)
	}
	if len(sources) != 1 {
		return usageErr(cmd, errors.New("discord export requires exactly one guild"))
	}
	source := sources[0]
	lastSync, err := st.GetLastSuccessfulSync(source.ID)
	if err != nil {
		if errors.Is(err, store.ErrSyncRunNotFound) {
			return fmt.Errorf(
				"discord guild %q has no successful Discord sync; run 'msgvault sync-discord %s' first",
				selector, source.Identifier,
			)
		}
		return fmt.Errorf("get last successful Discord sync: %w", err)
	}
	if !lastSync.CompletedAt.Valid {
		return fmt.Errorf("successful Discord sync for %q has no completion time", selector)
	}

	containers, err := st.DiscordExport(cmd.Context(), source.ID, start, end)
	if err != nil {
		return err
	}
	if err := validateDiscordExport(containers, start, end); err != nil {
		return err
	}
	name := source.Identifier
	if source.DisplayName.Valid && strings.TrimSpace(source.DisplayName.String) != "" {
		name = source.DisplayName.String
	}
	envelope := discordExportEnvelope{
		Schema:                discordExportSchema,
		MsgvaultVersion:       Version,
		SourceSyncCompletedAt: lastSync.CompletedAt.Time.UTC(),
		Guild:                 discordExportGuild{ID: source.Identifier, Name: name},
		Start:                 start,
		End:                   end,
		Containers:            containers,
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return fmt.Errorf("encode Discord export: %w", err)
	}
	return nil
}

func parseDiscordExportBounds(startValue, endValue string) (time.Time, time.Time, error) {
	start, err := time.Parse(time.RFC3339, startValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"invalid --start %q (expected RFC3339): %w", startValue, err,
		)
	}
	end, err := time.Parse(time.RFC3339, endValue)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"invalid --end %q (expected RFC3339): %w", endValue, err,
		)
	}
	start = start.UTC()
	end = end.UTC()
	if !start.Before(end) {
		return time.Time{}, time.Time{}, errors.New("--start must be before --end")
	}
	return start, end, nil
}

func validateDiscordExport(
	containers []store.DiscordExportContainer,
	start, end time.Time,
) error {
	containerIDs := make(map[string]struct{}, len(containers))
	messageIDs := make(map[string]struct{})
	for _, container := range containers {
		if container.ID == "" || container.Name == "" {
			return errors.New("discord export contains a container with incomplete required fields")
		}
		if container.Kind != "channel" && container.Kind != "thread" {
			return fmt.Errorf("discord container %s has invalid kind %q", container.ID, container.Kind)
		}
		if _, duplicate := containerIDs[container.ID]; duplicate {
			return fmt.Errorf("discord export contains duplicate container %s", container.ID)
		}
		containerIDs[container.ID] = struct{}{}
		for _, message := range container.Messages {
			if message.ID == "" || message.AuthorHandle == "" {
				return fmt.Errorf(
					"discord container %s contains a message with incomplete required fields",
					container.ID,
				)
			}
			if message.SentAt.Before(start) || !message.SentAt.Before(end) {
				return fmt.Errorf(
					"discord message %s falls outside the requested interval", message.ID,
				)
			}
			if _, duplicate := messageIDs[message.ID]; duplicate {
				return fmt.Errorf("discord export contains duplicate message %s", message.ID)
			}
			messageIDs[message.ID] = struct{}{}
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newExportDiscordCmd(defaultDiscordCommandDeps()))
}
