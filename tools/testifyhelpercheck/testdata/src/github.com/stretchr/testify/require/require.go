package require

import (
	"testing"
	"time"
)

type Assertions struct{}
type CollectT struct{}

func New(*testing.T) *Assertions                                                                   { return &Assertions{} }
func NoError(*testing.T, error, ...any)                                                            {}
func NotNil(*testing.T, any, ...any)                                                               {}
func (*Assertions) NoError(error, ...any)                                                          {}
func (*Assertions) NotNil(any, ...any)                                                             {}
func Eventually(*testing.T, func() bool, time.Duration, time.Duration, ...any)                     {}
func Eventuallyf(*testing.T, func() bool, time.Duration, time.Duration, string, ...any)            {}
func EventuallyWithT(*testing.T, func(*CollectT), time.Duration, time.Duration, ...any)            {}
func EventuallyWithTf(*testing.T, func(*CollectT), time.Duration, time.Duration, string, ...any)   {}
func Never(*testing.T, func() bool, time.Duration, time.Duration, ...any)                          {}
func Neverf(*testing.T, func() bool, time.Duration, time.Duration, string, ...any)                 {}
func (*Assertions) Eventually(func() bool, time.Duration, time.Duration, ...any)                   {}
func (*Assertions) Eventuallyf(func() bool, time.Duration, time.Duration, string, ...any)          {}
func (*Assertions) EventuallyWithT(func(*CollectT), time.Duration, time.Duration, ...any)          {}
func (*Assertions) EventuallyWithTf(func(*CollectT), time.Duration, time.Duration, string, ...any) {}
func (*Assertions) Never(func() bool, time.Duration, time.Duration, ...any)                        {}
func (*Assertions) Neverf(func() bool, time.Duration, time.Duration, string, ...any)               {}
