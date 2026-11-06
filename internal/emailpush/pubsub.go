package emailpush

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	MaxPullMessages     = 10
	MaxResponseBytes    = 128 << 10
	MaxAckIDBytes       = 4096
	MaxCredentialsBytes = 1 << 20
	requestTimeout      = 30 * time.Second
	pubsubURL           = "https://pubsub.googleapis.com/v1/"
	subscriberScope     = "https://www.googleapis.com/auth/pubsub"
)

var (
	ErrNotValidated     = errors.New("Pub/Sub subscription has not been validated")
	subscriptionPattern = regexp.MustCompile(`^projects/[A-Za-z0-9][A-Za-z0-9.:-]{0,127}/subscriptions/[A-Za-z][A-Za-z0-9._~+-]{0,254}$`)
	topicPattern        = regexp.MustCompile(`^projects/[A-Za-z0-9][A-Za-z0-9.:-]{0,127}/topics/[A-Za-z][A-Za-z0-9._~+-]{0,254}$`)
)

// ValidationError identifies unsupported configuration without including
// subscription identifiers, credentials or provider response bodies.
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return "invalid Pub/Sub configuration: " + e.Reason }

// HTTPError reports status without retaining a provider response body.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Pub/Sub request failed (HTTP %d)", e.StatusCode)
}

type requestError struct{ cause error }

func (e *requestError) Error() string { return "Pub/Sub request failed" }
func (e *requestError) Unwrap() error { return e.cause }

// Delivery retains the broker ACK token and opaque base64 data for classification.
// A missing ACK token rejects the Pull batch; malformed data remains classifiable.
type Delivery struct{ AckID, Data string }

// PubSub accesses one subscription exclusively assigned to one Gmail account.
// Config validation cannot detect another consumer; callers enforce exclusive
// ownership and source identity before starting. Reconciliation and ACK policy
// belong to the caller. No method retries or extends a delivery lease.
type PubSub struct {
	client              *http.Client
	subscription, topic string
	validated           atomic.Bool
}

// NewPubSub uses an explicit HTTP client, allowing source-controlled credentials
// and synthetic transports. Validate must succeed before Pull or Acknowledge.
func NewPubSub(client *http.Client, subscription, topic string) (*PubSub, error) {
	if client == nil {
		return nil, &ValidationError{Reason: "HTTP client is required"}
	}
	if !subscriptionPattern.MatchString(subscription) || !topicPattern.MatchString(topic) {
		return nil, &ValidationError{Reason: "subscription and topic resources are required"}
	}
	return &PubSub{client: client, subscription: subscription, topic: topic}, nil
}

// NewPubSubFromCredentials reads only the explicitly supplied service-account or
// authorized-user JSON file. It never discovers default credentials, opens an
// authentication flow or modifies credentials. Token requests are also bounded.
func NewPubSubFromCredentials(ctx context.Context, path, subscription, topic string) (*PubSub, error) {
	ps, err := NewPubSub(&http.Client{}, subscription, topic)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, &ValidationError{Reason: "subscriber credential file is required"}
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, &ValidationError{Reason: "subscriber credential file is unavailable"}
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxCredentialsBytes+1))
	if err != nil || len(data) > MaxCredentialsBytes {
		return nil, &ValidationError{Reason: "subscriber credential file is unreadable or too large"}
	}
	var credentialType struct {
		Type google.CredentialsType `json:"type"`
	}
	if err := json.Unmarshal(data, &credentialType); err != nil || (credentialType.Type != google.ServiceAccount && credentialType.Type != google.AuthorizedUser) {
		return nil, &ValidationError{Reason: "subscriber requires service-account or authorized-user JSON"}
	}
	tokenCtx := context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: requestTimeout})
	credentials, err := google.CredentialsFromJSONWithType(tokenCtx, data, credentialType.Type, subscriberScope)
	if err != nil {
		return nil, &ValidationError{Reason: "subscriber credential JSON is invalid"}
	}
	ps.client = oauth2.NewClient(tokenCtx, credentials.TokenSource)
	ps.client.Timeout = requestTimeout
	return ps, nil
}

// Validate requires active, ordinary at-least-once Pull delivery on the exact
// configured topic, without filters, transforms or dead-letter diversion. Any
// failed validation revokes admission until a later validation succeeds.
func (p *PubSub) Validate(ctx context.Context) (resultErr error) {
	p.validated.Store(false)
	defer func() {
		if resultErr != nil {
			p.validated.Store(false)
		}
	}()
	data, err := p.request(ctx, http.MethodGet, "", nil)
	if err != nil {
		return err
	}
	var config *struct {
		Name               string                    `json:"name"`
		Topic              string                    `json:"topic"`
		State              string                    `json:"state"`
		PushConfig         map[string]jsontext.Value `json:"pushConfig"`
		BigqueryConfig     *jsontext.Value           `json:"bigqueryConfig"`
		CloudStorageConfig *jsontext.Value           `json:"cloudStorageConfig"`
		BigtableConfig     *jsontext.Value           `json:"bigtableConfig"`
		ExactlyOnce        bool                      `json:"enableExactlyOnceDelivery"`
		Filter             string                    `json:"filter"`
		Transforms         []jsontext.Value          `json:"messageTransforms"`
		DeadLetterPolicy   *jsontext.Value           `json:"deadLetterPolicy"`
		Detached           bool                      `json:"detached"`
	}
	if err := json.Unmarshal(data, &config); err != nil || config == nil {
		return &ValidationError{Reason: "invalid subscription response"}
	}
	switch {
	case config.Name != p.subscription || config.Topic != p.topic:
		return &ValidationError{Reason: "subscription or topic does not match"}
	case config.State != "ACTIVE" || config.Detached:
		return &ValidationError{Reason: "subscription is not active"}
	case len(config.PushConfig) != 0 || config.BigqueryConfig != nil || config.CloudStorageConfig != nil || config.BigtableConfig != nil:
		return &ValidationError{Reason: "subscription must use Pull delivery"}
	case config.ExactlyOnce:
		return &ValidationError{Reason: "exactly-once delivery is unsupported"}
	case config.Filter != "" || len(config.Transforms) != 0 || config.DeadLetterPolicy != nil:
		return &ValidationError{Reason: "filters, transforms and dead-letter policies are unsupported"}
	}
	p.validated.Store(true)
	return nil
}

// Pull receives at most ten broker deliveries. Successful empty Pulls are not
// reconnects and do not imply a catchup. Data is parsed separately so malformed
// poison can be ACKed under the validated subscription ownership contract.
func (p *PubSub) Pull(ctx context.Context) ([]Delivery, error) {
	if !p.validated.Load() {
		return nil, ErrNotValidated
	}
	body, err := json.Marshal(struct {
		MaxMessages int `json:"maxMessages"`
	}{MaxPullMessages})
	if err != nil {
		return nil, &requestError{cause: err}
	}
	data, err := p.request(ctx, http.MethodPost, ":pull", body)
	if err != nil {
		return nil, err
	}
	var response *struct {
		Messages []struct {
			AckID   string `json:"ackId"`
			Message struct {
				Data string `json:"data"`
			} `json:"message"`
		} `json:"receivedMessages"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response == nil {
		return nil, errors.New("invalid Pub/Sub Pull response")
	}
	if len(response.Messages) > MaxPullMessages {
		return nil, errors.New("Pub/Sub Pull batch exceeds limit")
	}
	deliveries := make([]Delivery, len(response.Messages))
	for i, message := range response.Messages {
		if message.AckID == "" || len(message.AckID) > MaxAckIDBytes {
			return nil, errors.New("invalid Pub/Sub ACK token")
		}
		deliveries[i] = Delivery{AckID: message.AckID, Data: message.Message.Data}
	}
	return deliveries, nil
}

// Acknowledge sends only caller-approved ACK tokens. For valid Gmail hints the
// caller must first wait for complete source reconciliation and recheck source
// identity. Failed/partial/canceled catchup must leave deliveries unacknowledged.
func (p *PubSub) Acknowledge(ctx context.Context, ids []string) error {
	if !p.validated.Load() {
		return ErrNotValidated
	}
	if len(ids) > MaxPullMessages {
		return errors.New("Pub/Sub ACK batch exceeds limit")
	}
	for _, id := range ids {
		if id == "" || len(id) > MaxAckIDBytes {
			return errors.New("invalid Pub/Sub ACK token")
		}
	}
	if len(ids) == 0 {
		return nil
	}
	body, err := json.Marshal(struct {
		AckIDs []string `json:"ackIds"`
	}{ids})
	if err != nil {
		return &requestError{cause: err}
	}
	data, err := p.request(ctx, http.MethodPost, ":acknowledge", body)
	if err != nil {
		return err
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil || response == nil {
		return errors.New("invalid Pub/Sub ACK response")
	}
	return nil
}

func (p *PubSub) request(ctx context.Context, method, action string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, pubsubURL+p.subscription+action, bytes.NewReader(body))
	if err != nil {
		return nil, &requestError{cause: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, &requestError{cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, &requestError{cause: err}
	}
	if len(data) > MaxResponseBytes {
		return nil, errors.New("Pub/Sub response exceeds size limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode}
	}
	return data, nil
}
