package api

import "time"

func SetPersonTierHTTPClockForTest(s *Server, clock func() time.Time) { s.clock = clock }
func PersonTiersCLIRunAllowedForTest(args []string) bool              { return cliRunCommandAllowed(args) }
