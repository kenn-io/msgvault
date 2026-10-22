package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/zoomcalls"
)

type zoomOptions struct{ account, owner, uuid, roster, bindings string }
type zoomCounts struct {
	Upserted    int `json:"upserted"`
	Skipped     int `json:"skipped"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported,omitempty"`
}
type callOpener func(context.Context) (callCommandClient, func(), error)
type callWriter func(*cobra.Command, any) error

func zoomIdentityFlags(cmd *cobra.Command, o *zoomOptions) {
	cmd.Flags().StringVar(&o.account, "account", "", "Stable Zoom account/owner namespace (required)")
	cmd.Flags().StringVar(&o.owner, "owner-id", "", "Owner's exact Zoom native ID (required)")
	cmd.Flags().StringVar(&o.bindings, "bindings", "", "JSON file mapping native peers to existing archive participant IDs")
}

func readZoomFile(path string, limit int) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Zoom input: %w", err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, errors.New("read Zoom input")
	}
	if len(raw) > limit {
		return nil, errors.New("zoom input exceeds limit")
	}
	return raw, nil
}

func (o zoomOptions) bindingMap() (map[string]int64, error) {
	if o.account == "" || o.owner == "" {
		return nil, errors.New("zoom --account and --owner-id are required")
	}
	bindings := map[string]int64{}
	if o.bindings != "" {
		raw, err := readZoomFile(o.bindings, 64<<10)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &bindings); err != nil {
			return nil, errors.New("invalid Zoom bindings JSON")
		}
	}
	if len(bindings) > 100 {
		return nil, errors.New("zoom allows at most 100 participant bindings")
	}
	for key, id := range bindings {
		if key == "" || id <= 0 {
			return nil, errors.New("invalid Zoom binding")
		}
	}
	return bindings, nil
}

func addZoomCommands(root *cobra.Command, open callOpener, write callWriter) {
	var o zoomOptions
	cmd := &cobra.Command{Use: "import-zoom-meeting", Short: "Import confirmed attendance from a complete supplied Zoom roster", Args: cobra.NoArgs}
	zoomIdentityFlags(cmd, &o)
	cmd.Flags().StringVar(&o.uuid, "uuid", "", "Exact past meeting instance UUID (required)")
	cmd.Flags().StringVar(&o.roster, "roster", "", "Complete participant API response JSON file (required)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		bindings, err := o.bindingMap()
		if err != nil {
			return err
		}
		if o.roster == "" || o.uuid == "" {
			return errors.New("zoom --roster and --uuid are required")
		}
		raw, err := readZoomFile(o.roster, zoomcalls.MaxRosterBytes)
		if err != nil {
			return err
		}
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		observations, skipped, err := stageZoomMeetings(cmd.Context(), client, o, []zoomcalls.MeetingRecord{{UUID: o.uuid, Roster: raw}}, bindings)
		if err != nil {
			return err
		}
		return postZoomObservations(cmd, client, observations, zoomCounts{Skipped: skipped}, write)
	}
	addZoomFetchMode(cmd, &o, open, write)
	root.AddCommand(cmd)
	addZoomDiscoveryCommand(root, open, write)
	addZoomPhoneCommand(root, open, write)
}

// Stage complete rosters and reconciliation before the first import. A failed
// scan never erases previously archived attendance.
func stageZoomMeetings(ctx context.Context, client callCommandClient, o zoomOptions, records []zoomcalls.MeetingRecord, bindings map[string]int64) ([]callhistory.Observation, int, error) {
	mapped := make([]*callhistory.Observation, len(records))
	needed := map[string]bool{}
	for i, record := range records {
		observation, err := zoomcalls.Meeting(o.account, o.owner, record.UUID, record.Roster, bindings)
		if err != nil {
			return nil, 0, err
		}
		mapped[i] = observation
		if observation == nil {
			needed[record.UUID] = true
		}
	}
	var priors map[string]store.CallObservationView
	if len(needed) != 0 {
		var err error
		priors, err = priorZoomObservations(ctx, client, zoomcalls.AccountNamespace(o.account, o.owner), needed)
		if err != nil {
			return nil, 0, err
		}
	}
	var out []callhistory.Observation
	skipped := 0
	for i, record := range records {
		observation := mapped[i]
		if observation == nil {
			prior, ok := priors[record.UUID]
			if !ok {
				skipped++
				continue
			}
			evidence, err := zoomcalls.RosterEvidence(record.Roster)
			if err != nil {
				return nil, skipped, err
			}
			observation = &callhistory.Observation{Evidence: prior.Evidence, RecordID: record.UUID, Raw: evidence}
			observation.Outcome, observation.OwnerParticipation = "unknown", "unknown"
			observation.DurationSeconds = nil
		}
		if err := observation.Validate(); err != nil {
			return nil, skipped, err
		}
		out = append(out, *observation)
	}
	return out, skipped, nil
}

func priorZoomObservations(ctx context.Context, client callCommandClient, account string, needed map[string]bool) (map[string]store.CallObservationView, error) {
	found := map[string]store.CallObservationView{}
	var after int64
	for scanned := 0; scanned < 10000; {
		page, err := client.ListCallObservations(ctx, store.CallQuery{AfterID: after, Limit: 100})
		if err != nil {
			return nil, err
		}
		if len(page.Items) > 100 {
			return nil, errors.New("zoom reconciliation returned oversized metadata page")
		}
		if len(page.Items) == 0 {
			return found, nil
		}
		for _, item := range page.Items {
			if item.MessageID <= after {
				return nil, errors.New("zoom reconciliation cursor did not advance")
			}
			after = item.MessageID
			scanned++
			if item.Provider == "zoom_meetings" && item.Account == account && needed[item.RecordID] {
				found[item.RecordID] = item
				if len(found) == len(needed) {
					return found, nil
				}
			}
		}
		if len(page.Items) < 100 {
			return found, nil
		}
	}
	return nil, errors.New("zoom reconciliation exceeds 10000 observations; no evidence changed")
}

func postZoomObservations(cmd *cobra.Command, client callCommandClient, observations []callhistory.Observation, counts zoomCounts, write callWriter) error {
	for _, observation := range observations {
		record, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(callhistory.ImportRequest{Format: "normalized", Account: observation.Account, Record: record})
		if err != nil {
			return err
		}
		if _, err = client.ImportCallObservation(cmd.Context(), raw); err != nil {
			counts.Failed++
			if outputErr := write(cmd, counts); outputErr != nil {
				return outputErr
			}
			return fmt.Errorf("zoom import stopped after %d successful upserts: %w", counts.Upserted, err)
		}
		counts.Upserted++
	}
	return write(cmd, counts)
}
