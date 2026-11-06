package timeline

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

const cursorMaxBytes = 512

// CursorBinding contains the effective request and authorization inputs. SourceIDs
// are the source grant from the request context, not the set of returned sources.
// Window and Timezone must be resolved before decoding; decoding pins Now
// to the same instant, including nanoseconds, used on the first page.
type CursorBinding struct {
	// WindowAt resolves relative dates from the pinned Now during encoding and
	// decoding. The composer supplies its same pure window resolver on each page.
	WindowAt   func(time.Time) (Window, error)
	ArchiveUID string
	Request    Request
	SourceIDs  []int64
	Window     Window
	Timezone   string
	Now        time.Time
}
type Cursor struct {
	Window    Window
	Now       time.Time
	Positions map[string]string
}
type cursorWire struct {
	Version  int               `json:"v"`
	Archive  string            `json:"a"`
	Filter   string            `json:"f"`
	Now      int64             `json:"n"`
	Nanos    int               `json:"ns,omitzero"`
	Sections map[string]string `json:"s"`
}

// Continuation supplies Fit with source continuations and opaque sort keys in the
// exact order used by the composer. SortKeys prefer section+"|"+Entry.Key (or
// section+"|day:DATE" for a summary), falling back to Entry.Key for callers with
// section-independent ordering. Positions are the preceding page's keys. More marks unread
// source data; it is separate from budget omissions. Loops never continue.
type Continuation struct {
	Binding   CursorBinding
	Positions map[string]string
	More      map[string]bool
	SortKeys  map[string]string
}

// SetContinuation attaches request-local metadata; it is never part of JSON.
func SetContinuation(t *Timeline, c Continuation) {
	c.Positions = maps.Clone(c.Positions)
	c.More = maps.Clone(c.More)
	c.SortKeys = maps.Clone(c.SortKeys)
	c.Binding.SourceIDs = slices.Clone(c.Binding.SourceIDs)
	c.Binding.Request.Tiers = slices.Clone(c.Binding.Request.Tiers)
	c.Binding.Request.Scope.OpenTools = slices.Clone(c.Binding.Request.Scope.OpenTools)
	t.continuation = &c
}

func EncodeCursor(b CursorBinding, positions map[string]string) (string, error) {
	if b.ArchiveUID == "" || b.Now.IsZero() {
		return "", fmt.Errorf("%w: missing cursor binding", ErrInvalidRequest)
	}
	if b.WindowAt != nil {
		window, err := b.WindowAt(b.Now)
		if err != nil {
			return "", err
		}
		b.Window = window
	}
	archive, filter, err := cursorHashes(b)
	if err != nil {
		return "", err
	}
	sections := maps.Clone(positions)
	delete(sections, "open_loops")
	wire := cursorWire{Version: 1, Archive: archive, Filter: filter, Now: b.Now.Unix(), Nanos: b.Now.Nanosecond(), Sections: sections}
	encoded, err := store.EncodeReadCursor(wire, cursorMaxBytes, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("%w: cursor exceeds 512 bytes", ErrInvalidRequest)
	}
	return encoded, nil
}
func DecodeCursor(value string, b CursorBinding) (Cursor, error) {
	fail := func() (Cursor, error) { return Cursor{}, ErrCursorExpired }
	if b.ArchiveUID == "" || len(value) > base64.RawURLEncoding.EncodedLen(cursorMaxBytes) {
		return fail()
	}
	var wire cursorWire
	if err := store.DecodeReadCursor(value, &wire, cursorMaxBytes); err != nil {
		return fail()
	}
	if wire.Now <= 0 || wire.Now > 253402300799 || wire.Nanos < 0 || wire.Nanos >= 1000000000 {
		return fail()
	}
	now := time.Unix(wire.Now, int64(wire.Nanos)).UTC()
	if b.WindowAt != nil {
		window, err := b.WindowAt(now)
		if err != nil {
			return fail()
		}
		b.Window = window
	}
	archive, filter, err := cursorHashes(b)
	if err != nil || wire.Version != 1 || wire.Archive != archive || wire.Filter != filter || wire.Sections == nil {
		return fail()
	}
	if _, ok := wire.Sections["open_loops"]; ok {
		return fail()
	}
	return Cursor{Window: b.Window, Now: now, Positions: wire.Sections}, nil
}
func cursorHashes(b CursorBinding) (string, string, error) {
	req := b.Request
	req.Budget = 0
	req.Cursor = ""
	req.With = strings.TrimSpace(req.With)
	req.Around = ExpandShortRef(strings.TrimSpace(req.Around))
	if req.Mode == "" {
		req.Mode = ModeDay
		if req.With != "" {
			req.Mode = ModeRelationship
		}
		if req.Around != "" {
			req.Mode = ModeAnchor
		}
	}
	if req.Format == "" {
		req.Format = "markdown"
	}
	// Effective dates are carried by Window; equivalent flag spellings bind alike.
	req.Date = ""
	req.From = ""
	req.To = ""
	req.Days = 0
	if req.Before == 0 && !req.BeforeSet {
		req.Before = 5
	}
	if req.After == 0 && !req.AfterSet {
		req.After = 5
	}
	req.Tiers = slices.Compact(slices.Sorted(slices.Values(req.Tiers)))
	req.Scope.OpenTools = slices.Compact(slices.Sorted(slices.Values(req.Scope.OpenTools)))
	payload := struct {
		SourceScoped      bool
		OwnerAllOpenTools bool
		Request           Request
		SourceIDs         []int64
		Window            Window
		Timezone          string
	}{b.SourceIDs != nil, b.Request.Scope.Owner && b.Request.Scope.OpenTools == nil, req, slices.Compact(slices.Sorted(slices.Values(b.SourceIDs))), b.Window, b.Timezone}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	archive := sha256.Sum256([]byte(b.ArchiveUID))
	filter := sha256.Sum256(encoded)
	return hex.EncodeToString(archive[:4]), hex.EncodeToString(filter[:6]), nil
}
