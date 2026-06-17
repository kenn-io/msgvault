// Package tbmock provides a mock testing.TB for verifying fail-fast behavior.
package tbmock

import (
	"fmt"
	"testing"
)

// FatalSentinel is panicked by MockTB to halt execution, mimicking
// testing.TB methods that call runtime.Goexit (Fatal, Fatalf, FailNow,
// Skip, Skipf, SkipNow). Recovered by ExpectFatal.
type FatalSentinel struct{ Msg string }

// MockTB wraps a real testing.TB so that un-overridden methods delegate safely,
// while intercepting all fail/skip methods via a panic sentinel.
type MockTB struct {
	testing.TB // delegate to a real TB for methods we don't override

	failed   bool
	FatalMsg string
}

// NewMockTB creates a new MockTB wrapping a real testing.TB.
func NewMockTB(tb testing.TB) *MockTB {
	tb.Helper()
	return &MockTB{TB: tb}
}

// Failed returns whether a fatal/skip method was called.
func (f *MockTB) Failed() bool { return f.failed }

func (f *MockTB) Helper()                           {}
func (f *MockTB) Errorf(format string, args ...any) {}
func (f *MockTB) Cleanup(fn func())                 {}
func (f *MockTB) Fatalf(format string, args ...any) {
	f.failed = true
	f.FatalMsg = fmt.Sprintf(format, args...)
	panic(FatalSentinel{f.FatalMsg})
}
func (f *MockTB) Fatal(args ...any) {
	f.failed = true
	f.FatalMsg = fmt.Sprint(args...)
	panic(FatalSentinel{f.FatalMsg})
}
func (f *MockTB) FailNow() {
	f.failed = true
	f.FatalMsg = ""
	panic(FatalSentinel{})
}
func (f *MockTB) Skip(args ...any) {
	f.failed = true
	f.FatalMsg = fmt.Sprint(args...)
	panic(FatalSentinel{f.FatalMsg})
}
func (f *MockTB) Skipf(format string, args ...any) {
	f.failed = true
	f.FatalMsg = fmt.Sprintf(format, args...)
	panic(FatalSentinel{f.FatalMsg})
}
func (f *MockTB) SkipNow() {
	f.failed = true
	f.FatalMsg = ""
	panic(FatalSentinel{})
}

// ExpectFatal calls fn and recovers if it triggered a MockTB fatal/skip.
func ExpectFatal(mtb *MockTB, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(FatalSentinel); !ok {
				panic(r) // re-panic non-sentinel
			}
		}
	}()
	fn()
}
