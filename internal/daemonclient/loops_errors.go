package daemonclient

import "go.kenn.io/msgvault/internal/loops"

// ErrLoopsStale marks a refused ranking. LoopsStaleError retains its complete
// freshness response for CLI and MCP consumers through errors.As.
var ErrLoopsStale = loops.ErrLoopsStale

type LoopsStaleError = loops.StaleError
