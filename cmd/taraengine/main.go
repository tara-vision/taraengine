// Command taraengine is the Tara Vision inference engine.
package main

import (
	"fmt"
	"os"

	"github.com/tara-vision/taraengine/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "taraengine:", err)
		os.Exit(1)
	}
}
