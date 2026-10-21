package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/zoomcalls"
)

func addZoomDiscoveryCommand(root *cobra.Command, open callOpener, write callWriter) {
	var o zoomOptions
	var tokenCommand, from, to string
	cmd := &cobra.Command{Use: "import-zoom-meetings", Short: "Import the owner's accessible hosted Zoom meeting attendance", Args: cobra.NoArgs}
	zoomIdentityFlags(cmd, &o)
	cmd.Flags().StringVar(&tokenCommand, "token-command", "", "Token command JSON argv; requires report:read:user:admin and meeting:read:list_past_participants:admin")
	cmd.Flags().StringVar(&from, "from", "", "Inclusive YYYY-MM-DD (required)")
	cmd.Flags().StringVar(&to, "to", "", "Exclusive YYYY-MM-DD (required)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		argv, err := zoomTokenCommand(tokenCommand)
		if err != nil {
			return err
		}
		bindings, err := o.bindingMap()
		if err != nil {
			return err
		}
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		records, unsupported, err := zoomcalls.NewClient(argv).Meetings(cmd.Context(), o.owner, from, to)
		if err != nil {
			return err
		}
		observations, skipped, err := stageZoomMeetings(cmd.Context(), client, o, records, bindings)
		if err != nil {
			return err
		}
		return postZoomObservations(cmd, client, observations, zoomCounts{Skipped: skipped, Unsupported: unsupported}, write)
	}
	root.AddCommand(cmd)
}
