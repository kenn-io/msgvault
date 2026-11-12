package main

import (
	"context"

	"go.kenn.io/msgvault/internal/testutil/secretcommand"
)

func main() { secretcommand.Run(context.Background()) }
