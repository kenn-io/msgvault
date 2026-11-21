package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/placeautocomplete"
)

func init() { rootCmd.AddCommand(newAddressLookupCommand()) }

func newAddressLookupCommand() *cobra.Command {
	group := &cobra.Command{Use: "address", Short: "Find address suggestions with Google Places"}
	var token, language, region string
	var limit int
	var latitude, longitude, radius float64
	lookup := &cobra.Command{
		Use: "autocomplete <partial-address>", Short: "Request address candidates as JSON", Args: cobra.ExactArgs(1),
		Long: "Send the supplied partial address to Google Places. Results are suggestions, not validated addresses. Reuse the returned session token for subsequent requests and a single selected-place lookup.",
		RunE: func(cmd *cobra.Command, args []string) error {
			requestToken := token
			if requestToken == "" {
				var err error
				requestToken, err = placeautocomplete.NewSessionToken()
				if err != nil {
					return err
				}
			}
			var bias *placeautocomplete.Bias
			latSet, lngSet, radiusSet := cmd.Flags().Changed("lat"), cmd.Flags().Changed("lng"), cmd.Flags().Changed("radius")
			if latSet || lngSet || radiusSet {
				if !latSet || !lngSet || !radiusSet {
					return errors.New("--lat, --lng and --radius must be supplied together")
				}
				bias = &placeautocomplete.Bias{Latitude: latitude, Longitude: longitude, RadiusMeters: radius}
			}
			client := placeautocomplete.Client{Key: os.Getenv(placeautocomplete.APIKeyEnv)}
			result, err := client.Autocomplete(cmd.Context(), placeautocomplete.Request{Input: args[0], SessionToken: requestToken, Language: language, Region: region, Limit: limit, Bias: bias})
			if err != nil {
				return err
			}
			if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("write address JSON: %w", err)
			}
			return nil
		},
	}
	lookup.Flags().StringVar(&token, "session-token", "", "Reuse a token from a previous autocomplete request")
	lookup.Flags().StringVar(&language, "language", "", "Preferred language code")
	lookup.Flags().StringVar(&region, "region", "", "Preferred two-letter region code")
	lookup.Flags().IntVar(&limit, "limit", 5, "Maximum suggestions (1–5)")
	lookup.Flags().Float64Var(&latitude, "lat", 0, "Location bias latitude")
	lookup.Flags().Float64Var(&longitude, "lng", 0, "Location bias longitude")
	lookup.Flags().Float64Var(&radius, "radius", 0, "Location bias radius in metres")
	var selectToken, selectLanguage, selectRegion string
	selectCmd := &cobra.Command{
		Use: "select <place-id>", Short: "Fetch address details for a selected candidate as JSON", Args: cobra.ExactArgs(1),
		Long: "Fetch address-only Details for one explicitly selected candidate. Supply the autocomplete session token; a successful Details request ends that session.",
		RunE: func(cmd *cobra.Command, args []string) error {
			client := placeautocomplete.Client{Key: os.Getenv(placeautocomplete.APIKeyEnv)}
			result, err := client.Select(cmd.Context(), args[0], selectToken, selectLanguage, selectRegion)
			if err != nil {
				return err
			}
			if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("write address JSON: %w", err)
			}
			return nil
		},
	}
	selectCmd.Flags().StringVar(&selectToken, "session-token", "", "Autocomplete session token (required)")
	selectCmd.Flags().StringVar(&selectLanguage, "language", "", "Preferred language code")
	selectCmd.Flags().StringVar(&selectRegion, "region", "", "Preferred two-letter region code")
	group.AddCommand(lookup, selectCmd)
	return group
}
