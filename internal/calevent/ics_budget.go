package calevent

import (
	"context"
	"errors"
	"time"

	ical "github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/calrecur"
)

// ICSParseBudget bounds retained components across all calendars in one email.
// A file import uses its own budget.
type ICSParseBudget struct {
	bytes, components int
	// VerificationRemaining shares recurrence work with the caller's request.
	VerificationRemaining *int
}

func (b *ICSParseBudget) nextVerification(ctx context.Context, next rrule.Next) (time.Time, bool, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, false, err
	}
	if *b.VerificationRemaining <= 0 {
		return time.Time{}, false, calrecur.ErrBudgetExceeded
	}
	wall, ok := next()
	if ok {
		*b.VerificationRemaining--
	}
	return wall, ok, nil
}

func NewICSParseBudget() *ICSParseBudget {
	return &ICSParseBudget{bytes: MaxICSBytes, components: maxICSEvents, VerificationRemaining: new(calrecur.RequestReadBudget)}
}

func (b *ICSParseBudget) preflight(raw []byte) (string, error) {
	if b.components == 0 {
		return "", errors.New("calendar exceeds 10000 VEVENT limit")
	}
	if b.bytes == 0 {
		return "", errors.New("calendar retained event data exceeds 32 MiB limit")
	}
	return preflightICS(raw)
}

func (b *ICSParseBudget) retain(doc icsDocument, cal *ical.Calendar, raw []byte, index int) ([]byte, error) {
	if b.components == 0 {
		return nil, errors.New("calendar exceeds 10000 VEVENT limit")
	}
	retained, err := doc.retain(cal, raw, index, b.bytes)
	if err != nil {
		b.bytes = 0
		return nil, err
	}
	b.bytes -= len(retained)
	b.components--
	return retained, nil
}
