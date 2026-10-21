package cmd

import (
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/zoomcalls"
)

func addZoomPhoneCommand(root *cobra.Command, open callOpener, write callWriter) {
	var o zoomOptions
	var tokenCommand, from, to string
	cmd := &cobra.Command{Use: "import-zoom-phone", Short: "Import the owner's licensed Zoom Phone call elements", Args: cobra.NoArgs}
	zoomIdentityFlags(cmd, &o)
	cmd.Flags().StringVar(&tokenCommand, "token-command", "", "Token command JSON argv; requires phone:read:list_call_logs")
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
		items, err := zoomcalls.NewClient(argv).Phone(cmd.Context(), from, to)
		if err != nil {
			return err
		}
		observations := make([]callhistory.Observation, 0, len(items))
		for _, raw := range items {
			observation, err := zoomcalls.PhoneObservation(o.account, o.owner, raw, bindings)
			if err != nil {
				return err
			}
			observations = append(observations, observation)
		}
		return postZoomObservations(cmd, client, observations, zoomCounts{}, write)
	}
	root.AddCommand(cmd)
}
