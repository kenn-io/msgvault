package daemonclient

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/query"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// SenderAuthDiagnostics requests bounded counts and untrusted observations.
// The daemon requires owner credentials and never returns message content.
func (c *Client) SenderAuthDiagnostics(ctx context.Context, sourceID int64, limit int, suggest bool) (*query.SenderAuthDiagnostics, error) {
	sampleLimit := int64(limit)
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetSenderAuthDiagnosticsResp, error) {
		return client.GetSenderAuthDiagnosticsWithResponse(ctx, &generated.GetSenderAuthDiagnosticsRequestOptions{Query: &generated.GetSenderAuthDiagnosticsQuery{SourceID: sourceID, Limit: &sampleLimit, Suggest: &suggest}})
	})
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.JSON200 == nil {
		return nil, errors.New("daemon returned no sender-auth diagnostics")
	}
	body := resp.JSON200
	out := &query.SenderAuthDiagnostics{SourceID: body.SourceID, SampleLimit: int(body.SampleLimit), Scanned: int(body.Scanned), HasMore: body.HasMore, Verified: int(body.Verified), BasisCounts: make(map[string]int, len(body.BasisCounts)), Observations: make([]query.AuthservObservation, 0, len(body.Observations))}
	for basis, count := range body.BasisCounts {
		out.BasisCounts[basis] = int(count)
	}
	for _, observed := range body.Observations {
		out.Observations = append(out.Observations, query.AuthservObservation{AuthservID: observed.AuthservID, Count: int(observed.Count), ContentTrust: observed.ContentTrust})
	}
	return out, nil
}
