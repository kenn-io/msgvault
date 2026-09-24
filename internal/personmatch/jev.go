package personmatch

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

const (
	PacketSchemaVersion = PacketSchema
	jevUserAgent        = "msgvault-identity-scoring/1"
)

type PairEndpoint struct {
	Kind        string           `json:"kind"`
	DisplayName string           `json:"display_name,omitempty"`
	Email       string           `json:"email,omitempty"`
	Phone       string           `json:"phone,omitempty"`
	Identifiers []PairIdentifier `json:"identifiers,omitempty"`
}

type PairIdentifier struct {
	Type        string `json:"type"`
	Value       string `json:"value"`
	ServiceSlug string `json:"service_slug,omitempty"`
	ScopeKind   string `json:"scope_kind,omitempty"`
	ScopeValue  string `json:"scope_value,omitempty"`
}

type Evidence struct {
	Class       string `json:"class"`
	Value       string `json:"value,omitempty"`
	SourceCount int    `json:"source_count"`
}

// PairPacket is the entire provider disclosure. Freeform notes, messages,
// conversation text, and whole profiles never enter this type.
type PairPacket struct {
	SchemaVersion string       `json:"schema_version"`
	Left          PairEndpoint `json:"left"`
	Right         PairEndpoint `json:"right"`
	Evidence      []Evidence   `json:"evidence"`
}

type Judgment struct {
	Probability float64 `json:"probability"`
	ModelID     string  `json:"model_id"`
}

type JevClient struct {
	Endpoint    string
	ModelID     string
	Key         string
	HTTPClient  *http.Client
	RequestGate func(context.Context, func() error) (bool, error)
}

var ErrConsentRevoked = errors.New("person match consent was revoked before provider retry")

func (c JevClient) Score(ctx context.Context, packet PairPacket) (Judgment, error) {
	if c.Key == "" {
		return Judgment{}, errors.New("jev credential unavailable")
	}
	if c.Endpoint == "" || c.ModelID == "" {
		return Judgment{}, errors.New("jev endpoint and pinned model are required")
	}
	packet.SchemaVersion = PacketSchemaVersion
	requestBody, err := json.Marshal(struct {
		State     PairPacket     `json:"state"`
		Model     string         `json:"model"`
		Questions map[string]any `json:"questions"`
	}{State: packet, Model: c.ModelID, Questions: map[string]any{
		"same_person": map[string]any{
			"type":         "noul",
			"instructions": "Do left and right refer to the same individual human based only on the supplied identity evidence?",
			"criteria": map[string]string{
				"true":  "Independent identity evidence corroborates one human",
				"false": "Distinct people, including shared households, shared or recycled contacts, or similar names alone",
			},
		},
	}})
	if err != nil {
		return Judgment{}, errors.New("encode Jev request")
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client = &clientCopy
	for attempt := range 3 {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(requestBody))
		if err != nil {
			return Judgment{}, errors.New("create Jev request")
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", jevUserAgent)
		var statusCode int
		var data []byte
		var responseErr error
		dispatch := func() error {
			resp, dispatchErr := client.Do(req)
			if dispatchErr != nil {
				return dispatchErr
			}
			statusCode = resp.StatusCode
			data, responseErr = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if closeErr := resp.Body.Close(); responseErr == nil {
				responseErr = closeErr
			}
			return nil
		}
		allowed := true
		if c.RequestGate != nil {
			allowed, err = c.RequestGate(ctx, dispatch)
		} else {
			err = dispatch()
		}
		if err != nil {
			if ctx.Err() != nil {
				return Judgment{}, ctx.Err()
			}
			return Judgment{}, errors.New("jev request failed")
		}
		if !allowed {
			return Judgment{}, ErrConsentRevoked
		}
		if responseErr != nil {
			return Judgment{}, errors.New("read Jev response")
		}
		if statusCode == http.StatusTooManyRequests || statusCode == 529 {
			if attempt == 2 {
				return Judgment{}, errors.New("jev rate limit or overload retry exhausted")
			}
			select {
			case <-ctx.Done():
				return Judgment{}, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * 100 * time.Millisecond):
			}
			continue
		}
		if statusCode != http.StatusOK {
			return Judgment{}, fmt.Errorf("jev returned HTTP %d", statusCode)
		}
		var decoded struct {
			Model   string `json:"model"`
			Answers map[string]struct {
				Type string   `json:"type"`
				Noul *float64 `json:"noul"`
			} `json:"answers"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			return Judgment{}, errors.New("malformed Jev response")
		}
		answer, ok := decoded.Answers["same_person"]
		if decoded.Model != c.ModelID || !ok || answer.Type != "noul" || answer.Noul == nil ||
			math.IsNaN(*answer.Noul) || math.IsInf(*answer.Noul, 0) || *answer.Noul < 0 || *answer.Noul > 1 {
			return Judgment{}, errors.New("jev model or Noul answer failed validation")
		}
		return Judgment{Probability: *answer.Noul, ModelID: decoded.Model}, nil
	}
	return Judgment{}, errors.New("jev retry exhausted")
}
