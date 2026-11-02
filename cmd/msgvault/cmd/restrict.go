package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

const restrictionStagedNotice = "Staged configuration only; rules do not yet restrict reads."

type restrictionDeps struct {
	open func(context.Context) (*store.Store, func(), error)
}

func newRestrictCmd() *cobra.Command {
	cmd := newRestrictLocalCmd(restrictionDeps{open: func(ctx context.Context) (*store.Store, func(), error) {
		state := invocationFromContext(ctx)
		if state == nil || state.cfg == nil {
			return nil, nil, errors.New("configuration is unavailable")
		}
		return openWritableStoreAndInitForIngestInvocation(state)
	}})
	for _, child := range cmd.Commands() {
		local := child.RunE
		child.RunE = func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return local(cmd, args)
		}
	}
	return cmd
}
func newRestrictLocalCmd(deps restrictionDeps) *cobra.Command {
	cmd := &cobra.Command{Use: "restrict", Short: "Manage staged agent restriction rules", Long: restrictionStagedNotice + " Label names select current labels only, not future labels with the same name."}
	var reveal, note string
	var sourceID int64
	add := &cobra.Command{Use: "add KIND TARGET", Short: "Stage a label name, source ID, or person ID rule", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		st, closeStore, err := deps.open(cmd.Context())
		if err != nil {
			return err
		}
		defer closeStore()
		r := store.AgentRestriction{Kind: args[0], Reveal: reveal, Note: note}
		var ids []int64
		if r.Kind == "label" {
			ids, err = st.ResolveRestrictionLabels(cmd.Context(), args[1], sourceID)
		} else {
			if sourceID != 0 {
				return errors.New("--source applies only to label selectors")
			}
			var id int64
			id, err = strconv.ParseInt(args[1], 10, 64)
			ids = []int64{id}
		}
		if err != nil {
			return fmt.Errorf("resolve target: %w", err)
		}
		if len(ids) != 1 {
			return errors.New("label name is ambiguous; use --source to select a single label")
		}
		rules := make([]store.AgentRestriction, 0, len(ids))
		for _, id := range ids {
			switch r.Kind {
			case "label":
				r.LabelID = id
			case "source":
				r.SourceID = id
			case "person":
				r.PersonID = id
			}
			saved, err := st.AddAgentRestriction(cmd.Context(), r)
			if err != nil {
				return err
			}
			rules = append(rules, saved)
		}
		return printRestrictionResult(cmd, rules)
	}}
	add.Flags().StringVar(&reveal, "reveal", "headers", "staged reveal level: minimal, headers, or headers_subject")
	add.Flags().StringVar(&note, "note", "", "owner note")
	add.Flags().Int64Var(&sourceID, "source", 0, "limit label name lookup to a source ID")
	cmd.AddCommand(add)
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List staged rules", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		st, closeStore, err := deps.open(cmd.Context())
		if err != nil {
			return err
		}
		defer closeStore()
		rules, err := st.ListAgentRestrictions(cmd.Context())
		if err != nil {
			return err
		}
		return printRestrictionResult(cmd, rules)
	}})
	cmd.AddCommand(&cobra.Command{Use: "remove ID", Short: "Remove a staged rule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("ID must be a positive integer")
		}
		st, closeStore, err := deps.open(cmd.Context())
		if err != nil {
			return err
		}
		defer closeStore()
		if err = st.RemoveAgentRestriction(cmd.Context(), id); err != nil {
			return err
		}
		return printRestrictionResult(cmd, nil)
	}})
	cmd.AddCommand(&cobra.Command{Use: "check MESSAGE_ID", Short: "Show staged rules matching a message", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return errors.New("MESSAGE_ID must be a positive integer")
		}
		st, closeStore, err := deps.open(cmd.Context())
		if err != nil {
			return err
		}
		defer closeStore()
		rules, err := st.CheckAgentRestrictions(cmd.Context(), id)
		if err != nil {
			return err
		}
		return printRestrictionResult(cmd, rules)
	}})
	cmd.AddCommand(&cobra.Command{Use: "taint on|off", Short: "Set staged thread taint", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] != "on" && args[0] != "off" {
			return errors.New("expected on or off")
		}
		st, closeStore, err := deps.open(cmd.Context())
		if err != nil {
			return err
		}
		defer closeStore()
		if err = st.SetAgentRestrictionTaint(cmd.Context(), args[0] == "on"); err != nil {
			return err
		}
		return printRestrictionResult(cmd, args[0] == "on")
	}})
	return cmd
}
func printRestrictionResult(cmd *cobra.Command, value any) error {
	data, err := json.Marshal(struct {
		Staged bool   `json:"staged"`
		Notice string `json:"notice"`
		Result any    `json:"result"`
	}{true, restrictionStagedNotice, value})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	if err != nil {
		return fmt.Errorf("write restriction result: %w", err)
	}
	return nil
}
func init() { rootCmd.AddCommand(newRestrictCmd()) }
