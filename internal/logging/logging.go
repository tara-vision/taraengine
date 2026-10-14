// Package logging sets up the daemon's logger: a rotating file in the data directory and text on stderr.
package logging

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Setup returns the logger and a function that closes the file. The file rotates at 20 MB, three kept, and takes every
// line from info level up. stderr takes the lines from stderrLevel up: info for a daemon run by hand, warn for one a
// service manager runs, whose captured stderr would otherwise copy the whole log and never rotate.
func Setup(dataDir string, stderrLevel slog.Level) (*slog.Logger, func() error, error) {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("logging: %w", err)
	}
	file := &lumberjack.Logger{Filename: filepath.Join(dir, "taraengine.log"), MaxSize: 20, MaxBackups: 3}
	handler := slog.NewMultiHandler(
		slog.NewTextHandler(file, &slog.HandlerOptions{Level: slog.LevelInfo}),
		slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: stderrLevel}),
	)
	return slog.New(handler), file.Close, nil
}
