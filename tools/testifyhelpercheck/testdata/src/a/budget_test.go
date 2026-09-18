package a

import (
	"testing"
	"time"
	tm "time"

	aliasedAssert "github.com/stretchr/testify/assert"
	aliasedRequire "github.com/stretchr/testify/require"
)

func TestPollingBudgets(t *testing.T) {
	aliasedAssert.Eventually(t, func() bool { return true }, 999*time.Millisecond, time.Millisecond) // want "assert.Eventually budget 999ms"
	// 0.5*time.Second is rejected because time.Second is a typed duration constant.
	aliasedRequire.Eventually(t, func() bool { return true }, 0.5*1e9*time.Nanosecond, time.Millisecond)    // want "require.Eventually budget 500ms"
	aliasedRequire.Neverf(t, func() bool { return false }, 0, time.Millisecond, "never")                    // want "require.Neverf budget 0s"
	aliasedAssert.EventuallyWithT(t, func(*aliasedAssert.CollectT) {}, -time.Millisecond, time.Millisecond) // want "assert.EventuallyWithT budget -1ms"
	aliasedRequire.Eventually(t, func() bool { return true }, time.Second, time.Millisecond)
	aliasedAssert.Eventually(t, func() bool { return true }, namedBudget, time.Millisecond)
	aliasedRequire.Eventually(t, func() bool { return true }, variableBudget(), time.Millisecond)
	aliasedAssert.Eventually(t, func() bool { return true }, time.Second, time.Millisecond)                    // want "test has 9 direct testify package calls"
	aliasedRequire.EventuallyWithTf(t, func(*aliasedRequire.CollectT) {}, time.Second, time.Millisecond, "ok") // want "test has 9 direct testify package calls"

	assertions := aliasedAssert.New(t)
	assertions.Eventuallyf(func() bool { return true }, 999*time.Millisecond, time.Millisecond, "poll") // want "assert.Eventuallyf budget 999ms"
	requirements := aliasedRequire.New(t)
	requirements.Never(func() bool { return false }, time.Second, time.Millisecond)
	callWithAssertions(t, assertions, requirements)
}

func callWithAssertions(t *testing.T, assertions *aliasedAssert.Assertions, requirements *aliasedRequire.Assertions) {
	assertions.Eventually(func() bool { return true }, 0, time.Millisecond) // want "assert.Eventually budget 0s"
	requirements.Neverf(func() bool { return false }, time.Second, time.Millisecond, "ok")
}

func helperOutsideTest(t *testing.T) {
	aliasedAssert.Eventually(t, func() bool { return true }, 999*time.Millisecond, time.Millisecond) // want "assert.Eventually budget 999ms"
}

func variableBudget() time.Duration { return time.Millisecond }

const namedBudget = 999 * time.Millisecond

func TestNegativeSpace(t *testing.T) {
	aliasedAssert.Eventually(t, func() bool { return true }, namedBudget, time.Millisecond)
	aliasedAssert.Eventually(t, func() bool { return true }, variableBudget(), time.Millisecond)
	time.Sleep(time.Millisecond)
	_ = "assert.Eventually(t, f, 999*time.Millisecond)"
}

func TestAliasedTimeBudget(t *testing.T) {
	aliasedAssert.Eventually(t, func() bool { return true }, 999*tm.Millisecond, tm.Millisecond) // want "assert.Eventually budget 999ms"
}

func TestShadowedTimeImport(t *testing.T) {
	time := struct{ Millisecond time.Duration }{}
	aliasedAssert.Eventually(t, func() bool { return true }, time.Millisecond, time.Millisecond)
}

type unrelatedAssertions struct{}

func (unrelatedAssertions) Eventually(*testing.T, func() bool, time.Duration, time.Duration) {}

func TestUnrelatedEventuallyMethod(t *testing.T) {
	var assertions unrelatedAssertions
	assertions.Eventually(t, func() bool { return true }, 999*time.Millisecond, time.Millisecond)
}
