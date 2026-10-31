package imazingcsv

// These tests load named time zones. Builds use -trimpath, so Go cannot fall
// back to the zone database in GOROOT, and hosts without a system database,
// such as Windows, need this embedded copy.
import _ "time/tzdata"
