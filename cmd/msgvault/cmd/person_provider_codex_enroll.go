package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/tui"
)

type codexEnrollDeps struct {
	openBackend  func(context.Context) (tui.PeopleInferenceBackend, func(), error)
	isTerminal   func(*cobra.Command) bool
	pollInterval time.Duration
}

type codexEnrollOptions struct {
	model            string
	reasoningEffort  string
	retentionPosture string
	trainingPosture  string
	allowedSources   []string
	sourceSince      string
	sourceUntil      string
	allowSensitive   bool
	yes              bool
}

func defaultCodexEnrollDeps() codexEnrollDeps {
	return codexEnrollDeps{
		openBackend: func(ctx context.Context) (tui.PeopleInferenceBackend, func(), error) {
			client, _, err := OpenHTTPStore(ctx)
			if err != nil {
				return nil, nil, err
			}
			return newTUISettingsBackend(client), func() { _ = client.Close() }, nil
		},
		isTerminal:   commandStdinIsTerminal,
		pollInterval: time.Second,
	}
}

func newPersonProviderCodexEnrollCommand(deps codexEnrollDeps) *cobra.Command {
	var options codexEnrollOptions
	command := &cobra.Command{
		Use:   "enroll-codex <name>",
		Short: "Create and select a Codex people provider through the daemon",
		Args:  exactPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return runPersonProviderCodexEnroll(command, deps, args[0], options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.model, "model", "", "Codex model ID (prompt when omitted)")
	flags.StringVar(&options.reasoningEffort, "reasoning-effort", "", "Supported reasoning effort (prompt when omitted)")
	flags.StringVar(&options.retentionPosture, "retention-posture", "", "Operator retention assertion")
	flags.StringVar(&options.trainingPosture, "training-posture", "", "Operator training assertion")
	flags.StringSliceVar(&options.allowedSources, "source", nil, "Archive source class (repeatable)")
	flags.StringVar(&options.sourceSince, "source-since", "", "Earliest disclosed source date (YYYY-MM-DD)")
	flags.StringVar(&options.sourceUntil, "source-until", "", "Latest disclosed source date (YYYY-MM-DD)")
	flags.BoolVar(&options.allowSensitive, "allow-sensitive", false, "Explicitly allow or exclude sensitive archive content")
	flags.BoolVar(&options.yes, "yes", false, "Confirm the displayed check disclosure and select the profile")
	return command
}

func runPersonProviderCodexEnroll(
	command *cobra.Command, deps codexEnrollDeps, name string, options codexEnrollOptions,
) error {
	if deps.isTerminal == nil || !deps.isTerminal(command) {
		return errors.New("codex enrollment requires a terminal; noninteractive commands never start device login")
	}
	if !command.Flags().Changed("allow-sensitive") || strings.TrimSpace(options.retentionPosture) == "" ||
		strings.TrimSpace(options.trainingPosture) == "" || len(options.allowedSources) == 0 {
		return errors.New("codex enrollment requires --retention-posture, --training-posture, --source, and --allow-sensitive=true|false")
	}
	for _, source := range options.allowedSources {
		if !slices.Contains([]string{"conversation_text", "meeting_text", "document_text"}, source) {
			return fmt.Errorf("unsupported source class %q", source)
		}
	}
	since, err := time.Parse("2006-01-02", options.sourceSince)
	if err != nil {
		return errors.New("--source-since must be a valid YYYY-MM-DD date")
	}
	if options.sourceUntil != "" {
		until, err := time.Parse("2006-01-02", options.sourceUntil)
		if err != nil || until.Before(since) {
			return errors.New("--source-until must be a valid date on or after --source-since")
		}
	}
	if deps.openBackend == nil {
		return errors.New("daemon people enrollment client is unavailable")
	}
	backend, closeBackend, err := deps.openBackend(command.Context())
	if err != nil {
		return err
	}
	if closeBackend != nil {
		defer closeBackend()
	}
	if backend == nil {
		return errors.New("daemon people enrollment client is unavailable")
	}
	ctx := command.Context()
	login, err := backend.StartCodexLogin(ctx, name)
	if err != nil {
		return err
	}
	sessionActive := true
	defer func() {
		if sessionActive {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = backend.CancelCodexLogin(cancelCtx, login.SessionID)
		}
	}()
	out := command.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Verification URL: %s\nUser code: %s\nLocal deadline: %s\n",
		login.URL, login.Code, login.Deadline.UTC().Format(time.RFC3339))
	interval := deps.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if !login.Deadline.IsZero() && time.Now().After(login.Deadline) {
			return errors.New("codex device login reached its local deadline")
		}
		poll, err := backend.PollCodexLogin(ctx, login.SessionID)
		if err != nil {
			return err
		}
		if poll.Complete {
			break
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	models, err := backend.ListCodexModels(ctx, login.SessionID)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(command.InOrStdin())
	model, effort, err := chooseCodexEnrollmentModel(out, reader, models, options.model, options.reasoningEffort)
	if err != nil {
		return err
	}
	profile, err := backend.SaveCodexProfile(ctx, login.SessionID, tui.CodexProfileRequest{
		Name: name, Model: model, ReasoningEffort: effort,
		RetentionPosture: strings.TrimSpace(options.retentionPosture),
		TrainingPosture:  strings.TrimSpace(options.trainingPosture),
		AllowedSources:   append([]string(nil), options.allowedSources...),
		SourceSince:      options.sourceSince, SourceUntil: options.sourceUntil,
		AllowSensitive: options.allowSensitive,
	})
	if err != nil {
		return err
	}
	sessionActive = false // Profile creation consumes the daemon login draft.
	disclosure, err := backend.CheckCodexProfile(ctx, profile)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Synthetic check passed for %s (fingerprint %s).\n%s\n",
		profile, disclosure.Fingerprint, disclosure.Text)
	if !options.yes {
		confirmed, err := promptCodexEnrollmentYes(reader, out, "Grant consent and select this profile? [y/N]: ")
		if err != nil {
			return err
		}
		if !confirmed {
			return errors.New("consent declined; profile was saved but not selected")
		}
	}
	if err := backend.ConsentCodexProfile(ctx, disclosure.Profile, disclosure.Fingerprint); err != nil {
		return err
	}
	if err := backend.SelectCodexProfile(ctx, profile); err != nil {
		return err
	}
	status, err := backend.LoadPeopleInferenceStatus(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Selected profile: %s\nConfigured: %s  Running: %s\n",
		profile, status.Configured, status.Running)
	if status.PendingRestart {
		_, _ = fmt.Fprintln(out, "Restart the daemon to use the selected profile.")
	}
	return nil
}

func chooseCodexEnrollmentModel(
	out io.Writer, reader *bufio.Reader, models []tui.CodexModelChoice, modelID, effort string,
) (string, string, error) {
	if len(models) == 0 {
		return "", "", errors.New("signed-in Codex account returned no models")
	}
	if modelID == "" {
		_, _ = fmt.Fprintln(out, "Available Codex models:")
		for _, model := range models {
			_, _ = fmt.Fprintf(out, "  %s (reasoning: %s)\n", model.ID, strings.Join(model.ReasoningEfforts, ", "))
		}
		_, _ = fmt.Fprint(out, "Model ID: ")
		answer, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", fmt.Errorf("read model choice: %w", err)
		}
		modelID = strings.TrimSpace(answer)
	}
	for _, model := range models {
		if model.ID != modelID {
			continue
		}
		if effort == "" {
			_, _ = fmt.Fprintf(out, "Reasoning effort for %s (%s; default %s): ",
				model.ID, strings.Join(model.ReasoningEfforts, ", "), model.DefaultReasoningEffort)
			answer, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return "", "", fmt.Errorf("read reasoning effort: %w", err)
			}
			effort = strings.TrimSpace(answer)
			if effort == "" {
				effort = model.DefaultReasoningEffort
			}
		}
		if !slices.Contains(model.ReasoningEfforts, effort) {
			return "", "", fmt.Errorf("reasoning effort %q is unavailable for model %q", effort, modelID)
		}
		return modelID, effort, nil
	}
	return "", "", fmt.Errorf("model %q is unavailable for the signed-in Codex account", modelID)
}

func promptCodexEnrollmentYes(reader *bufio.Reader, out io.Writer, prompt string) (bool, error) {
	_, _ = fmt.Fprint(out, prompt)
	answer, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read consent answer: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}
