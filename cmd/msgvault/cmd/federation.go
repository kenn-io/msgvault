package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func init() {
	root := &cobra.Command{Use: "federation", Short: "Manage hub enrollment"}
	identity := &cobra.Command{Use: "identity", Short: "Print this archive's federation UID", Long: "Print this archive's federation UID. Set [federation] role to spoke or hub and restart the daemon first.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := OpenHTTPStore(cmd.Context(), "5.1.0")
		if err != nil {
			return err
		}
		out, err := c.FederationIdentity(cmd.Context())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), out.ArchiveUID)
		return nil
	}}
	var spoke, member, name string
	enroll := &cobra.Command{Use: "enroll", Short: "Enroll a spoke on this hub", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if spoke == "" || member == "" {
			return errors.New("--spoke-instance and --member are required")
		}
		c, _, err := OpenHTTPStore(cmd.Context(), "5.1.0")
		if err != nil {
			return err
		}
		result, err := c.CreateFederationEnrollment(cmd.Context(), spoke, member, name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), result.Token)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Enrollment: %s\nMember: %s\n", result.Enrollment.EnrollmentUID, result.Enrollment.Member)
		if result.Enrollment.SpokeName != result.Enrollment.Member {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Spoke: %s\n", result.Enrollment.SpokeName)
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Join (upcoming, not in this release): msgvault federation join <hub-url> --hub-instance %s --token-file <path>\n", result.HubInstance)
		return nil
	}}
	enroll.Flags().StringVar(&spoke, "spoke-instance", "", "Spoke archive UID (64 lowercase hexadecimal digits)")
	enroll.Flags().StringVar(&member, "member", "", "Member handle (any non-blank name without control characters)")
	enroll.Flags().StringVar(&name, "spoke-name", "", "Spoke display name (no control characters)")
	list := &cobra.Command{Use: "list", Short: "List spoke enrollments", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := OpenHTTPStore(cmd.Context(), "5.1.0")
		if err != nil {
			return err
		}
		entries, err := c.ListFederationEnrollments(cmd.Context())
		if err != nil {
			return err
		}
		for _, e := range entries {
			state := accountStateActive
			if e.RevokedAt != nil {
				state = "revoked"
			}
			names := e.Member
			if e.SpokeName != e.Member {
				names += " (" + e.SpokeName + ")"
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", e.EnrollmentUID, names, state)
		}
		return nil
	}}
	enrollments := &cobra.Command{Use: "enrollments", Short: "List enrollments"}
	enrollments.AddCommand(list)
	revoke := &cobra.Command{Use: "revoke <enrollment-uid>", Short: "Revoke a spoke enrollment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(args[0]) == "" {
			return errors.New("enrollment UID is required")
		}
		c, _, err := OpenHTTPStore(cmd.Context(), "5.1.0")
		if err != nil {
			return err
		}
		if err = c.RevokeFederationEnrollment(cmd.Context(), args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Enrollment revoked.")
		return nil
	}}
	root.AddCommand(identity, enroll, enrollments, revoke)
	rootCmd.AddCommand(root)
}
