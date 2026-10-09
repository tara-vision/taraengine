// Package logging sets up the daemon's logger: text on stderr and a rotating file in the data directory.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Setup returns the logger and a function that closes the file. The file rotates at 20 MB, three kept.
func Setup(dataDir string) (*slog.Logger, func() error, error) {
	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, fmt.Errorf("logging: %w", err)
	}
	file := &lumberjack.Logger{Filename: filepath.Join(dir, "taraengine.log"), MaxSize: 20, MaxBackups: 3}
	handler := slog.NewTextHandler(io.MultiWriter(os.Stderr, file), &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(handler), file.Close, nil
}
