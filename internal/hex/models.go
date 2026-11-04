// Package hex reads Hex's meeting folders without modifying the source.
package hex

import "go.kenn.io/msgvault/internal/hex/evidence"

const SourceType = "hex"
const RawFormat = "hex_json"

type Manifest = evidence.Manifest
type Notes = evidence.Notes
type Summary = evidence.Summary
type Calendar = evidence.Calendar
type Chapter = evidence.Chapter
type Entry = evidence.Entry
type Meeting evidence.Meeting
