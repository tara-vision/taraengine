package cli

import (
	"fmt"
	"os"

	"github.com/tara-vision/taraengine/internal/config"
)

// configPath is the file the configuration comes from: the one named with --config, or the default location.
func configPath(flags *globalFlags) string {
	if flags.config != "" {
		return flags.config
	}
	return config.DefaultConfigPath()
}

// loadConfig reads the configuration. A file named with --config must exist, while the default location may be
// absent, in which case the defaults apply.
func loadConfig(flags *globalFlags) (config.Config, error) {
	if flags.config != "" {
		if _, err := os.Stat(flags.config); err != nil {
			return config.Config{}, fmt.Errorf("config: %w", err)
		}
	}
	return config.Load(configPath(flags))
}
