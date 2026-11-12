package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/netguard"
	"go.kenn.io/msgvault/internal/oauth"
)

func federationHubOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || (u.Fragment != "" || strings.Contains(raw, "#")) || (u.Scheme != "https" && !(u.Scheme == "http" && netguard.IsLoopbackHost(u.Hostname()))) {
		return "", errors.New("--hub-url must be an HTTPS origin (HTTP only for loopback)")
	}
	u.Path = ""
	return u.String(), nil
}
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
		_, err = fmt.Fprintln(cmd.OutOrStdout(), out.ArchiveUID)
		return err
	}}
	var spoke, member, name, hub string
	var asJSON bool
	enroll := &cobra.Command{Use: "enroll", Short: "Enroll a spoke on this hub", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if spoke == "" || member == "" {
			return errors.New("--spoke-instance and --member are required")
		}
		origin := "<hub-url>"
		if cmd.Flags().Changed("hub-url") {
			var err error
			origin, err = federationHubOrigin(hub)
			if err != nil {
				return err
			}
		}
		c, _, err := OpenHTTPStore(cmd.Context(), "5.1.0")
		if err != nil {
			return err
		}
		result, err := c.CreateFederationEnrollment(cmd.Context(), spoke, member, name)
		if err != nil {
			return err
		}
		if asJSON {
			if cmd.Flags().Changed("hub-url") {
				result.HubURL = origin
			}
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true))
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), result.Token); err != nil {
			return err
		}
		joinOrigin := origin
		if cmd.Flags().Changed("hub-url") {
			joinOrigin = oauth.ShellQuote(origin)
		}
		_, err = fmt.Fprintf(cmd.ErrOrStderr(), "Enrollment: %s\nMember: %s\nSpoke: %s\nJoin (upcoming, not in this release): msgvault federation join %s --hub-instance %s --token-file <path>\n", result.Enrollment.EnrollmentUID, result.Enrollment.Member, result.Enrollment.SpokeName, joinOrigin, result.HubInstance)
		return err
	}}
	enroll.Flags().StringVar(&spoke, "spoke-instance", "", "Spoke archive UID (64 lowercase hexadecimal digits)")
	enroll.Flags().StringVar(&member, "member", "", "Member handle: 1–64 lowercase ASCII letters, digits, dots, underscores or hyphens; start with a letter or digit")
	enroll.Flags().StringVar(&name, "spoke-name", "", "Spoke display name (up to 128 bytes; no control characters)")
	enroll.Flags().StringVar(&hub, "hub-url", "", "Public hub origin")
	enroll.Flags().BoolVar(&asJSON, "json", false, "Output JSON")
	var listJSON bool
	list := &cobra.Command{Use: "list", Short: "List spoke enrollments", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, _, err := OpenHTTPStore(cmd.Context(), "5.1.0")
		if err != nil {
			return err
		}
		entries, err := c.ListFederationEnrollments(cmd.Context())
		if err != nil {
			return err
		}
		if listJSON {
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), map[string]any{"enrollments": entries}, json.Deterministic(true))
		}
		for _, e := range entries {
			state := "active"
			if e.RevokedAt != nil {
				state = "revoked"
			}
			lastSeen := "-"
			if e.LastSeenAt != nil {
				lastSeen = e.LastSeenAt.UTC().Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", e.EnrollmentUID, e.Member, e.SpokeName, state, lastSeen)
		}
		return nil
	}}
	list.Flags().BoolVar(&listJSON, "json", false, "Output JSON")
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
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Enrollment revoked.")
		return err
	}}
	root.AddCommand(identity, enroll, enrollments, revoke)
	rootCmd.AddCommand(root)
}
