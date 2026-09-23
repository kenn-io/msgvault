package daemonclient

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/vector/visual"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const visualSearchLimitField = "limit"

func (c *Client) SearchVisualAttachments(ctx context.Context, text string, image []byte, limit int) (*visual.SearchResponse, error) {
	return c.SearchVisualAttachmentsFiltered(ctx, VisualSearchOptions{Text: text, Image: image, Limit: limit})
}

type VisualSearchOptions struct {
	Text           string
	Image          []byte
	Limit          int
	Cursor         string
	SenderPersonID int64
	PersonID       int64
	ParticipantID  int64
	Directions     []personscope.Direction
	SourceID       int64
	MessageID      int64
	Filename       string
	MIMEPrefix     string
	After          *time.Time
	Before         *time.Time
}

func (c *Client) SearchVisualAttachmentsFiltered(ctx context.Context, options VisualSearchOptions) (*visual.SearchResponse, error) {
	var body bytes.Buffer
	contentType := "application/json"
	if len(options.Image) > 0 {
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("image", "query-image")
		if err != nil {
			return nil, fmt.Errorf("create visual image form part: %w", err)
		}
		if _, err := part.Write(options.Image); err != nil {
			return nil, err
		}
		if options.Limit > 0 {
			_ = writer.WriteField(visualSearchLimitField, strconv.Itoa(options.Limit))
		}
		if options.SenderPersonID > 0 {
			_ = writer.WriteField("sender_person_id", strconv.FormatInt(options.SenderPersonID, 10))
		}
		if options.PersonID > 0 {
			_ = writer.WriteField("person_id", strconv.FormatInt(options.PersonID, 10))
		}
		if options.ParticipantID > 0 {
			_ = writer.WriteField("participant_id", strconv.FormatInt(options.ParticipantID, 10))
		}
		for _, direction := range options.Directions {
			_ = writer.WriteField("direction", string(direction))
		}
		for key, value := range map[string]string{
			"cursor": options.Cursor, "filename": options.Filename, "mime_prefix": options.MIMEPrefix,
		} {
			if value != "" {
				_ = writer.WriteField(key, value)
			}
		}
		if options.SourceID > 0 {
			_ = writer.WriteField("source_id", strconv.FormatInt(options.SourceID, 10))
		}
		if options.MessageID > 0 {
			_ = writer.WriteField("message_id", strconv.FormatInt(options.MessageID, 10))
		}
		if options.After != nil {
			_ = writer.WriteField("after", options.After.UTC().Format(time.RFC3339Nano))
		}
		if options.Before != nil {
			_ = writer.WriteField("before", options.Before.UTC().Format(time.RFC3339Nano))
		}
		if err := writer.Close(); err != nil {
			return nil, fmt.Errorf("close visual search form: %w", err)
		}
		contentType = writer.FormDataContentType()
	} else {
		payload := map[string]any{
			"text": options.Text, visualSearchLimitField: options.Limit, "cursor": options.Cursor,
			"sender_person_id": options.SenderPersonID, "source_id": options.SourceID,
			"person_id": options.PersonID, "participant_id": options.ParticipantID,
			"directions": options.Directions,
			"message_id": options.MessageID, "filename": options.Filename, "mime_prefix": options.MIMEPrefix,
		}
		if options.After != nil {
			payload["after"] = options.After.UTC().Format(time.RFC3339Nano)
		}
		if options.Before != nil {
			payload["before"] = options.Before.UTC().Format(time.RFC3339Nano)
		}
		if err := json.MarshalWrite(&body, payload, json.Deterministic(true)); err != nil {
			return nil, err
		}
	}
	request := &generated.SearchVisualAttachmentsRequestOptions{}
	var editors []apiclient.RequestEditorFn
	if len(options.Image) > 0 {
		editors = append(editors, func(_ context.Context, req *http.Request) error {
			req.Body = io.NopCloser(bytes.NewReader(body.Bytes()))
			req.ContentLength = int64(body.Len())
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body.Bytes())), nil
			}
			req.Header.Set("Content-Type", contentType)
			return nil
		})
	} else {
		request.Body = new(generated.SearchVisualAttachmentsBody)
		if err := json.Unmarshal(body.Bytes(), request.Body); err != nil {
			return nil, err
		}
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SearchVisualAttachmentsResp, error) {
		return client.SearchVisualAttachmentsWithResponse(ctx, request, editors...)
	})
	if err != nil {
		return nil, err
	}
	var result visual.SearchResponse
	if err := json.Unmarshal(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("decode visual attachment search: %w", err)
	}
	return &result, nil
}

// VisualStatusWithCoverage fetches visual status with the per-format coverage
// scan, which re-reads every candidate blob; the daemon serializes it.
func (c *Client) VisualStatusWithCoverage(ctx context.Context) (*visual.Status, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetVisualAttachmentStatusResp, error) {
		return client.GetVisualAttachmentStatusWithResponse(ctx, func(_ context.Context, req *http.Request) error {
			query := req.URL.Query()
			query.Set("coverage", "1")
			req.URL.RawQuery = query.Encode()
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	var status visual.Status
	if err := json.Unmarshal(response.Body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (c *Client) RunVisualBuildPass(ctx context.Context) (*visual.Status, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ResumeVisualAttachmentBuildResp, error) {
		return client.ResumeVisualAttachmentBuildWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	var status visual.Status
	if err := json.Unmarshal(response.Body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (c *Client) ConsentVisualBuildPass(ctx context.Context) (*visual.Status, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.StartVisualAttachmentBuildResp, error) {
		return client.StartVisualAttachmentBuildWithResponse(ctx, &generated.StartVisualAttachmentBuildRequestOptions{Body: &generated.StartVisualAttachmentBuildBody{Consent: true}})
	})
	if err != nil {
		return nil, err
	}
	var status visual.Status
	if err := json.Unmarshal(response.Body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (c *Client) RetryVisualOwner(ctx context.Context, messageID int64, blobHash string) (*visual.Status, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.RetryVisualAttachmentOwnerResp, error) {
		return client.RetryVisualAttachmentOwnerWithResponse(ctx, &generated.RetryVisualAttachmentOwnerRequestOptions{Body: &generated.RetryVisualAttachmentOwnerBody{MessageID: messageID, BlobHash: blobHash}})
	})
	if err != nil {
		return nil, err
	}
	var status visual.Status
	if err := json.Unmarshal(response.Body, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (c *Client) RetireVisualGeneration(ctx context.Context, generationID int64) error {
	_, err := APIResponseWithStatuses(c, []int{http.StatusNoContent}, func(client *apiclient.Client) (*generated.RetireVisualAttachmentGenerationResp, error) {
		return client.RetireVisualAttachmentGenerationWithResponse(ctx, &generated.RetireVisualAttachmentGenerationRequestOptions{
			Body: &generated.RetireVisualAttachmentGenerationBody{GenerationID: generationID},
		})
	})
	return err
}
