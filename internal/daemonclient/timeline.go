package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/jsonexact"
	"go.kenn.io/msgvault/internal/timeline"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// timelineHTTPResponse keeps the original bytes for both supported encodings.
// In particular, decoding Open.Args through the generated map[string]any would
// round integer message IDs above 2^53 before a CLI could write them again.
type timelineHTTPResponse struct {
	StatusCode int
	Body       []byte
}

// Timeline fetches a fitted timeline or its shared Markdown projection.
func (c *Client) Timeline(ctx context.Context, req timeline.Request) (*timeline.Timeline, string, error) {
	if req.Format == "" {
		req.Format = "markdown"
	}
	query := timelineQuery(req)
	resp, err := apiResponseWithErrorDecoder(c, func(client *apiclient.Client) (*timelineHTTPResponse, error) {
		response, err := client.GetTimelineWithResponse(ctx, &generated.GetTimelineRequestOptions{
			Query: &generated.GetTimelineQuery{Format: new(req.Format)},
		}, func(_ context.Context, r *http.Request) error {
			r.URL.RawQuery = query.Encode()
			return nil
		})
		if response == nil {
			return nil, err
		}
		return &timelineHTTPResponse{StatusCode: response.StatusCode, Body: response.Body}, err
	}, timelineErrorBody)
	if err != nil {
		return nil, "", err
	}
	if req.Format == "markdown" {
		return nil, string(resp.Body), nil
	}
	var result timeline.Timeline
	if err := json.Unmarshal(resp.Body, &result, jsonexact.PreserveNumbers); err != nil {
		return nil, "", fmt.Errorf("decode timeline: %w", err)
	}
	return &result, "", nil
}

func timelineQuery(req timeline.Request) url.Values {
	q := url.Values{}
	for name, value := range map[string]string{
		"mode": string(req.Mode), "date": req.Date, "from": req.From, "to": req.To,
		"with": req.With, "around": req.Around, "format": req.Format, "cursor": req.Cursor,
	} {
		if value != "" {
			q.Set(name, value)
		}
	}
	for name, value := range map[string]int{"days": req.Days, "before": req.Before, "after": req.After, "budget": req.Budget} {
		if value != 0 || name == "before" && req.BeforeSet || name == "after" && req.AfterSet {
			q.Set(name, strconv.Itoa(value))
		}
	}
	for name, value := range map[string]bool{"conversation": req.Conversation, "include_cold": req.IncludeCold, "include_noise": req.IncludeNoise, "brief": req.Brief} {
		if value {
			q.Set(name, "true")
		}
	}
	if len(req.Tiers) > 0 {
		tiers := make([]string, len(req.Tiers))
		for i, tier := range req.Tiers {
			tiers[i] = strconv.Itoa(tier)
		}
		q.Set("tiers", strings.Join(tiers, ","))
	}
	return q
}

func timelineErrorBody(status int, body []byte) error {
	if status == http.StatusConflict {
		var response struct {
			Error      string               `json:"error"`
			Candidates []timeline.Candidate `json:"candidates"`
		}
		if json.Unmarshal(body, &response) == nil && response.Error == "ambiguous_reference" {
			return &timeline.ErrAmbiguousReference{Candidates: response.Candidates}
		}
	}
	return handleErrorBody(status, body)
}
