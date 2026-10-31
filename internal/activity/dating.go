// Package activity classifies and projects archived communication.
package activity

import (
	"go.kenn.io/msgvault/internal/activitydate"
	"time"
)

type DateOrigin = activitydate.DateOrigin
type DatePrecision = activitydate.DatePrecision
type Timestamps = activitydate.Timestamps
type Dating = activitydate.Dating

const (
	OriginSentAt       = activitydate.OriginSentAt
	OriginReceivedAt   = activitydate.OriginReceivedAt
	OriginInternalDate = activitydate.OriginInternalDate
	PrecisionTimestamp = activitydate.PrecisionTimestamp
	PrecisionDay       = activitydate.PrecisionDay
)

var ErrNoTimestamp = activitydate.ErrNoTimestamp

func LoadZone(name string) (*time.Location, error) { return activitydate.LoadZone(name) }
func DateEvent(ts Timestamps, loc *time.Location) (Dating, error) {
	return activitydate.DateEvent(ts, loc)
}
