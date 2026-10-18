package daemonclient

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"go.kenn.io/msgvault/internal/store"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// EmailFamilyContext resolves archive families through the owning daemon.
func (c *Client) EmailFamilyContext(ctx context.Context, id int64, options store.EmailFamilyOptions) (*store.EmailFamily, error) {
	path := fmt.Sprintf("/api/v1/messages/%d/family", id)
	if options.SourceIDs != nil {
		values := url.Values{}
		if len(options.SourceIDs) == 0 {
			values.Add("source_id", "")
		}
		for _, source := range options.SourceIDs {
			values.Add("source_id", strconv.FormatInt(source, 10))
		}
		path += "?" + values.Encode()
	}
	response, err := c.doGeneratedRequestWithHTTPClient(ctx, http.MethodGet, path, nil, c.httpClient, 1<<20)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil, store.ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get email family (HTTP %d)", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var family store.EmailFamily
	if err := json.Unmarshal(body, &family); err != nil {
		return nil, fmt.Errorf("decode email family: %w", err)
	}
	return &family, nil
}
