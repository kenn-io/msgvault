// Package ptr provides small value constructors for tests.
package ptr

import "time"

// Date returns a UTC time for the given year, month, and day.
func Date(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}
