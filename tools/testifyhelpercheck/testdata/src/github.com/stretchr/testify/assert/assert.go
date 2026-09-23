package assert

import (
	"testing"
	"time"
)

type Assertions struct{}
type CollectT struct{}

func New(*testing.T) *Assertions                                                    { return &Assertions{} }
func Equal(*testing.T, any, any, ...any) bool                                       { return true }
func True(*testing.T, bool, ...any) bool                                            { return true }
func (*Assertions) Equal(any, any, ...any) bool                                     { return true }
func (*Assertions) True(bool, ...any) bool                                          { return true }
func Eventually(*testing.T, func() bool, time.Duration, time.Duration, ...any) bool { return true }
func Eventuallyf(*testing.T, func() bool, time.Duration, time.Duration, string, ...any) bool {
	return true
}
func EventuallyWithT(*testing.T, func(*CollectT), time.Duration, time.Duration, ...any) bool {
	return true
}
func EventuallyWithTf(*testing.T, func(*CollectT), time.Duration, time.Duration, string, ...any) bool {
	return true
}
func Never(*testing.T, func() bool, time.Duration, time.Duration, ...any) bool          { return true }
func Neverf(*testing.T, func() bool, time.Duration, time.Duration, string, ...any) bool { return true }
func (*Assertions) Eventually(func() bool, time.Duration, time.Duration, ...any) bool   { return true }
func (*Assertions) Eventuallyf(func() bool, time.Duration, time.Duration, string, ...any) bool {
	return true
}
func (*Assertions) EventuallyWithT(func(*CollectT), time.Duration, time.Duration, ...any) bool {
	return true
}
func (*Assertions) EventuallyWithTf(func(*CollectT), time.Duration, time.Duration, string, ...any) bool {
	return true
}
func (*Assertions) Never(func() bool, time.Duration, time.Duration, ...any) bool { return true }
func (*Assertions) Neverf(func() bool, time.Duration, time.Duration, string, ...any) bool {
	return true
}
