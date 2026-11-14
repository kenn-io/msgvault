package signalexport

import (
	"context"
	"encoding/json/v2"
	"errors"
	"maps"
	"slices"
	"sort"
)

// maxHintIdentities caps the attachment identities a scan retains, about 200
// bytes each with bookkeeping, so a scan holds roughly 50 MiB at most. Frames
// past the cap get no hint and fall back to export order.
const maxHintIdentities = 1 << 18

// laterFrame is one export frame with attachment identities.
type laterFrame struct {
	line int
	ids  []string
}

// identityHints maps each same-time tuple to its frames that carry attachment
// identities, in export order. It lets an unmatched frame see whether a later
// frame owns a stored row before the unmatched frame takes that row.
type identityHints struct {
	frames map[string][]laterFrame
	count  int
}

func hintTuple(item *chatItem, date, received decimal) string {
	b, _ := json.Marshal([]string{string(item.ChatID), string(item.AuthorID), string(date), string(received)})
	return string(b)
}

// after returns the frames of tuple that follow line. It is safe on nil hints.
func (h *identityHints) after(tuple string, line int) []laterFrame {
	if h == nil {
		return nil
	}
	frames := h.frames[tuple]
	return frames[sort.Search(len(frames), func(i int) bool { return frames[i].line > line }):]
}

// carriedByOne reports whether exactly one frame carries an identity in have.
func carriedByOne(have map[string]bool, frames []laterFrame) bool {
	carriers := 0
	for _, f := range frames {
		if slices.ContainsFunc(f.ids, func(id string) bool { return have[id] }) {
			carriers++
		}
	}
	return carriers == 1
}

// scanIdentities reads the export once more with a separate reader and records
// chat-item tuples and attachment identities only. Invalid input ends the scan
// quietly; the import pass reports it with line context. Only cancellation is an error.
func scanIdentities(ctx context.Context, root string) (*identityHints, error) {
	h := &identityHints{frames: map[string][]laterFrame{}}
	if r, err := openReader(ctx, root); err == nil {
		_ = r.frames(ctx, func(m *chatItem, _ []byte, line int) error {
			date, ok := originalDate(m)
			if !ok || m.Standard == nil {
				return nil
			}
			next := laterFrame{line: line, ids: slices.Collect(maps.Keys(attachmentIdentities(m.Standard)))}
			if h.count += len(next.ids); h.count > maxHintIdentities {
				return errors.New("identity hint limit")
			}
			if len(next.ids) > 0 {
				tuple := hintTuple(m, date, originalReceived(m))
				h.frames[tuple] = append(h.frames[tuple], next)
			}
			return nil
		})
		_ = r.Close()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return h, nil
}
