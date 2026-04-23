// Command tray is the systray companion process. The real implementation
// arrives in A5 using getlantern/systray. For A1 this is a stub so the
// cmd/ layout and release pipeline are in place.
package main

import (
	"os"

	"github.com/vishal0589/gstreco-tally-agent/internal/log"
	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		os.Stdout.WriteString("gstreco-tally-tray " + version.String() + "\n")
		return
	}

	logger := log.New(log.Options{Console: true})
	logger.Info().Str("version", version.String()).Msg("tray stub — systray integration lands in A5")
}
