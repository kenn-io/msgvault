package docbankmedia

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
)

type ProcessingSelector struct {
	NodeID           int64  `json:"node_id"`
	ContentVersionID string `json:"content_version_id"`
	Profile          string `json:"profile"`
}

type ProcessingPlan struct {
	Fingerprint               string `json:"fingerprint"`
	RenditionIneligibleReason string `json:"rendition_ineligible_reason"`
}

func (c *Client) PlanProcessing(ctx context.Context, selector ProcessingSelector) (ProcessingPlan, error) {
	var plan ProcessingPlan
	err := c.jsonRequest(ctx, http.MethodPost, "/api/v1/processing/plans", struct {
		Selector ProcessingSelector `json:"selector"`
	}{selector}, &plan)
	if err == nil && plan.Fingerprint == "" {
		err = ErrInvalidReceipt
	}
	return plan, err
}

// StartProcessing reads the durable job identity and closes the stream before processing finishes.
// Consent must already have been granted in Docbank.
func (c *Client) StartProcessing(ctx context.Context, selector ProcessingSelector, fingerprint string) (string, error) {
	request, err := c.newJSONRequest(ctx, http.MethodPost, "/api/v1/processing/jobs", struct {
		Selector        ProcessingSelector `json:"selector"`
		PlanFingerprint string             `json:"plan_fingerprint"`
		Consent         bool               `json:"consent"`
	}{selector, fingerprint, false}, maxMetadataBytes)
	if err != nil {
		return "", err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return "", transportError(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		data, err := readResponse(response, maxResponseBytes)
		if err != nil {
			return "", err
		}
		return "", newHTTPError(response.StatusCode, data)
	}
	var event struct {
		Type string `json:"type"`
		Job  *struct {
			ID string `json:"id"`
		} `json:"job"`
	}
	if err := json.UnmarshalDecode(jsontext.NewDecoder(io.LimitReader(response.Body, maxMetadataBytes)), &event); err != nil {
		_, syntaxError := errors.AsType[*jsontext.SyntacticError](err)
		_, semanticError := errors.AsType[*json.SemanticError](err)
		if syntaxError || semanticError || errors.Is(err, io.EOF) {
			return "", ErrInvalidReceipt
		}
		return "", transportError(err)
	}
	if event.Type != "job" || event.Job == nil || event.Job.ID == "" {
		return "", ErrInvalidReceipt
	}
	return event.Job.ID, nil
}

// ProcessingProfile describes the extraction stage selected in Docbank.
type ProcessingProfile struct {
	Fingerprint string `json:"fingerprint"`
	Rendition   bool   `json:"rendition"`
}

func (c *Client) Profile(ctx context.Context, name string) (ProcessingProfile, error) {
	var profiles []struct {
		ProcessingProfile

		Name string `json:"name"`
	}
	if err := c.jsonRequest(ctx, http.MethodGet, "/api/v1/processing/profiles", nil, &profiles); err != nil {
		return ProcessingProfile{}, err
	}
	for _, profile := range profiles {
		if profile.Name == name {
			if profile.Fingerprint == "" {
				return ProcessingProfile{}, ErrInvalidReceipt
			}
			return profile.ProcessingProfile, nil
		}
	}
	return ProcessingProfile{}, fmt.Errorf("document_profile %q is not listed by Docbank", name)
}
