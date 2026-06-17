package main

import (
	"go.kenn.io/msgvault/tools/testifyhelpercheck"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(testifyhelpercheck.Analyzer)
}
