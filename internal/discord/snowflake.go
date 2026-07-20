package discord

import (
	"fmt"
	"strconv"
	"time"
)

const (
	discordEpochMilliseconds int64  = 1420070400000
	snowflakeSequenceBits    uint   = 22
	snowflakeSequenceMask    uint64 = (1 << snowflakeSequenceBits) - 1
)

// ParseSnowflake parses a Discord snowflake's unsigned decimal representation.
func ParseSnowflake(snowflake string) (uint64, error) {
	value, err := strconv.ParseUint(snowflake, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse Discord snowflake %q: %w", snowflake, err)
	}
	return value, nil
}

// TimestampFromSnowflake returns the UTC millisecond timestamp encoded in a
// Discord snowflake.
func TimestampFromSnowflake(snowflake string) (time.Time, error) {
	value, err := ParseSnowflake(snowflake)
	if err != nil {
		return time.Time{}, err
	}
	milliseconds := int64(value>>snowflakeSequenceBits) + discordEpochMilliseconds
	return time.UnixMilli(milliseconds).UTC(), nil
}

// SnowflakeFromTimestamp returns the lowest possible snowflake in the UTC
// millisecond containing timestamp.
func SnowflakeFromTimestamp(timestamp time.Time) (string, error) {
	lower, _, err := SnowflakeBoundsForTimestamp(timestamp)
	return lower, err
}

// SnowflakeBoundsForTimestamp returns the inclusive lower and upper snowflake
// bounds for the UTC millisecond containing timestamp.
func SnowflakeBoundsForTimestamp(timestamp time.Time) (string, string, error) {
	delta := timestamp.UTC().UnixMilli() - discordEpochMilliseconds
	if delta < 0 {
		return "", "", fmt.Errorf("timestamp %s predates the Discord epoch", timestamp.UTC().Format(time.RFC3339Nano))
	}
	if uint64(delta) > ^uint64(0)>>snowflakeSequenceBits {
		return "", "", fmt.Errorf("timestamp %s exceeds the Discord snowflake range", timestamp.UTC().Format(time.RFC3339Nano))
	}

	lower := uint64(delta) << snowflakeSequenceBits
	upper := lower | snowflakeSequenceMask
	return strconv.FormatUint(lower, 10), strconv.FormatUint(upper, 10), nil
}
