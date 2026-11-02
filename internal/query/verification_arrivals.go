package query

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/verification"
)

// ScanVerifications uses ordinary scoped list queries and primary-key body
// lookups. It never scans or joins message_bodies in a list query. The cap makes
// polling work explicit even in busy archives; classification is not stored.
func ScanVerifications(ctx context.Context, engine Engine, filter MessageFilter, options verification.ListOptions, now time.Time, reveal func(int64) (bool, error)) (*verification.ArrivalPage, error) {
	options, err := options.Normalize(now)
	if err != nil {
		return nil, err
	}
	page := &verification.ArrivalPage{Data: []verification.Arrival{}, Since: options.Since, Before: now}
	filter.ReceivedAfter, filter.ReceivedBefore = &options.Since, &now
	filter.Sorting = MessageSorting{Field: MessageSortByReceivedAt, Direction: SortDesc}
	const batchSize, scanCap = 100, 1000
	for page.Scanned < scanCap {
		filter.Pagination = Pagination{Limit: batchSize, Offset: page.Scanned}
		rows, err := engine.ListMessages(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page.Scanned++
			msg, err := engine.GetMessage(ctx, row.ID)
			if err != nil {
				return nil, err
			}
			if msg == nil || msg.ID != row.ID || msg.SourceID != row.SourceID {
				return nil, errors.New("canonical verification message unavailable")
			}
			in := VerificationInput(msg)
			receivedAt := in.EffectiveReceivedAt()
			if receivedAt.Before(options.Since) || receivedAt.After(now) {
				continue
			}
			finding := verification.Detect(in)
			if finding == nil || options.Kind != "" && finding.Kind != options.Kind {
				continue
			}
			allowed := options.Reveal
			if reveal != nil {
				allowed, err = reveal(msg.SourceID)
				if err != nil {
					return nil, err
				}
			}
			metadata := verification.Describe(in, finding, !allowed, nil, now)
			if metadata.Expired && !options.IncludeExpired {
				continue
			}
			// Filter using the actual sender claim, before masking its text.
			if options.Sender != "" && !strings.EqualFold(options.Sender, in.SenderEmail) && !strings.EqualFold(options.Sender, in.SenderPhone) {
				continue
			}
			arrival := verification.Arrival{MessageID: msg.ID, SourceID: msg.SourceID, SourceMessageID: msg.SourceMessageID, ConversationID: msg.ConversationID, Verification: metadata}
			if allowed {
				for _, secret := range finding.Values {
					arrival.Values = append(arrival.Values, verification.RevealedValue{Kind: secret.Kind, Value: secret.Value, Host: secret.Host})
				}
			}
			page.Data = append(page.Data, arrival)
			if len(page.Data) == options.Limit {
				page.ScanTruncated = true
				return page, nil
			}
		}
		if len(rows) < batchSize {
			return page, nil
		}
	}
	page.ScanTruncated = true
	return page, nil
}
