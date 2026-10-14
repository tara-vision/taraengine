// Command taraengine is the Tara Vision inference engine.
package main

import (
	"fmt"
	"os"

	"github.com/tara-vision/taraengine/internal/cli"
)

func main() {
	err := cli.Execute()
	if err != nil && err.Error() != "" {
		fmt.Fprintln(os.Stderr, "taraengine:", err)
	}
	os.Exit(cli.ExitCode(err))
}
