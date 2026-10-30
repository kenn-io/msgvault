package line

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const webhookPrefix = "/webhooks/line/"
const webhookMaxBytes = 1 << 20
const webhookIngressLimit = 8
const webhookBodyReadLimit = 16

// ReceiverChannel is one enabled, registered channel captured at daemon startup.
type ReceiverChannel struct {
	ChannelID string
	SourceID  int64
	BotUserID string
	SecretEnv string
	Secret    string `json:"-"`
}

// ActiveChannel proves which configuration the running receiver actually mounted.
// It intentionally excludes credentials.
type ActiveChannel struct {
	ChannelID string `json:"channel_id"`
	SourceID  int64  `json:"source_id"`
	BotUserID string `json:"bot_user_id"`
	SecretEnv string `json:"secret_env"`
}

type receiverChannel struct {
	config      ReceiverChannel
	invalid     atomic.Uint64
	lastWarning atomic.Int64
}

// Receiver authenticates and commits incoming events without calling LINE.
// Channel configuration is immutable; only signature counters change at runtime.
type Receiver struct {
	store     *store.Store
	channels  map[string]*receiverChannel
	ingress   chan struct{}
	bodyReads chan struct{}
	wake      func(string)
}

// NewReceiver takes enabled channels only. wake must enqueue work without waiting
// for processing; it receives the channel ID after a successful nonempty commit.
func NewReceiver(st *store.Store, channels []ReceiverChannel, wake func(string)) (*Receiver, error) {
	receiver := &Receiver{
		store: st, channels: make(map[string]*receiverChannel),
		ingress:   make(chan struct{}, webhookIngressLimit),
		bodyReads: make(chan struct{}, webhookBodyReadLimit), wake: wake,
	}
	if len(channels) > 0 && st == nil {
		return nil, errors.New("line: receiver requires an archive store")
	}
	for _, channel := range channels {
		if !config.ValidLineChannelID(channel.ChannelID) {
			return nil, errors.New("line: invalid receiver channel ID")
		}
		if channel.SourceID <= 0 || channel.BotUserID == "" || channel.Secret == "" {
			return nil, errors.New("line: receiver requires source, bot identity and secret")
		}
		if _, duplicate := receiver.channels[channel.ChannelID]; duplicate {
			return nil, errors.New("line: duplicate receiver channel ID")
		}
		receiver.channels[channel.ChannelID] = &receiverChannel{config: channel}
	}
	return receiver, nil
}

// Handles admits only a literal POST route with one nonempty channel segment.
// Unknown channels share this boundary and receive an opaque 404 in ServeHTTP.
func (r *Receiver) Handles(request *http.Request) bool {
	if r == nil || len(r.channels) == 0 || request.Method != http.MethodPost || request.URL == nil {
		return false
	}
	path := request.URL.Path
	if request.URL.RawPath != "" || request.URL.EscapedPath() != path || !strings.HasPrefix(path, webhookPrefix) {
		return false
	}
	channelID := strings.TrimPrefix(path, webhookPrefix)
	return channelID != "" && !strings.Contains(channelID, "/")
}

func (r *Receiver) ActiveChannels() []ActiveChannel {
	active := make([]ActiveChannel, 0, len(r.channels))
	for _, channel := range r.channels {
		cfg := channel.config
		active = append(active, ActiveChannel{ChannelID: cfg.ChannelID, SourceID: cfg.SourceID, BotUserID: cfg.BotUserID, SecretEnv: cfg.SecretEnv})
	}
	slices.SortFunc(active, func(a, b ActiveChannel) int { return strings.Compare(a.ChannelID, b.ChannelID) })
	return active
}

func (r *Receiver) InvalidSignatures(channelID string) uint64 {
	if channel := r.channels[channelID]; channel != nil {
		return channel.invalid.Load()
	}
	return 0
}

func (r *Receiver) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if !r.Handles(request) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	channel := r.channels[strings.TrimPrefix(request.URL.Path, webhookPrefix)]
	if channel == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Bound untrusted readers and retained bodies before allocating bytes. This
	// is separate from the smaller authenticated inbox-transaction allowance.
	select {
	case r.bodyReads <- struct{}{}:
		defer func() { <-r.bodyReads }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body := http.MaxBytesReader(w, request.Body, webhookMaxBytes)
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	if !Verify(channel.config.Secret, raw, request.Header.Get("X-Line-Signature")) {
		channel.invalid.Add(1)
		now := time.Now().Unix()
		previous := channel.lastWarning.Load()
		if now-previous >= 60 && channel.lastWarning.CompareAndSwap(previous, now) {
			slog.Warn("LINE webhook signature rejected", "channel_id", channel.config.ChannelID)
		}
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var envelope struct {
		Destination string         `json:"destination"`
		Events      jsontext.Value `json:"events"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if envelope.Destination == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if envelope.Destination != channel.config.BotUserID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Decode into a slice only after excluding missing/null/nonarray events.
	if len(envelope.Events) == 0 || envelope.Events[0] != '[' {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var events []jsontext.Value
	if err := json.Unmarshal(envelope.Events, &events); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	select {
	case r.ingress <- struct{}{}:
		defer func() { <-r.ingress }()
	default:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	batch := make([]store.LineWebhookEvent, 0, len(events))
	for _, rawEvent := range events {
		event, err := receiverEvent(rawEvent)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		batch = append(batch, event)
	}
	if len(batch) > 0 {
		if err := r.store.EnqueueLineEventsContext(request.Context(), channel.config.SourceID, channel.config.ChannelID, channel.config.BotUserID, batch); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	if len(batch) > 0 && r.wake != nil {
		r.wake(channel.config.ChannelID)
	}
}

func receiverEvent(raw jsontext.Value) (store.LineWebhookEvent, error) {
	clean, err := SanitizeEvent(raw)
	if err != nil {
		return store.LineWebhookEvent{}, err
	}
	// Future event types own their native payload shapes. Validate only common
	// routing fields until the type is known to require a message or unsend.
	var common struct {
		Type           string `json:"type"`
		WebhookEventID string `json:"webhookEventId"`
		Timestamp      int64  `json:"timestamp"`
	}
	if err := json.Unmarshal(clean, &common); err != nil {
		return store.LineWebhookEvent{}, errors.New("line: invalid webhook fields")
	}
	if common.Type == "" || common.WebhookEventID == "" || len(common.WebhookEventID) > 200 || strings.HasPrefix(common.WebhookEventID, "_") || common.Timestamp <= 0 {
		return store.LineWebhookEvent{}, errors.New("line: invalid webhook event")
	}
	target := ""
	switch common.Type {
	case "message", "messageEdited", "unsend":
		var event Event
		if err := json.Unmarshal(clean, &event); err != nil {
			return store.LineWebhookEvent{}, err
		}
		if common.Type == "unsend" {
			if event.Unsend == nil || event.Unsend.MessageID == "" {
				return store.LineWebhookEvent{}, errors.New("line: invalid unsend event")
			}
			target = event.Unsend.MessageID
		} else {
			if event.Message == nil || event.Message.ID == "" || event.Message.Type == "" {
				return store.LineWebhookEvent{}, errors.New("line: invalid message event")
			}
			target = event.Message.ID
		}
	}
	return store.LineWebhookEvent{WebhookEventID: common.WebhookEventID, EventType: common.Type, EventTimestamp: common.Timestamp, TargetMessageID: target, RawJSON: string(clean)}, nil
}
