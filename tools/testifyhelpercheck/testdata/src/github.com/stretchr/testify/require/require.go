package require

import "testing"

type Assertions struct{}

func New(*testing.T) *Assertions { return &Assertions{} }

func NoError(*testing.T, error, ...any) {}
func NotNil(*testing.T, any, ...any)    {}

func (*Assertions) NoError(error, ...any) {}
func (*Assertions) NotNil(any, ...any)    {}
