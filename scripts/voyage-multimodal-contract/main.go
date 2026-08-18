// Command voyage-multimodal-contract hosts opt-in, authenticated contract tests
// for the Voyage multimodal embedding API.
package main

import "fmt"

func main() {
	fmt.Println("run with: go test -tags voyage_contract ./scripts/voyage-multimodal-contract -count=1 -v")
}
