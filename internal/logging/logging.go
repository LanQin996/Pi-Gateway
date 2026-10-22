// Package logging configures the process-wide structured logger.
package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Setup installs a slog logger writing to stdout and, optionally, a file.
// It returns the logger plus a closer for the file sink.
func Setup(level, file string) (*slog.Logger, func() error, error) {
	var writers []io.Writer
	writers = append(writers, os.Stdout)

	var closer = func() error { return nil }
	if strings.TrimSpace(file) != "" {
		if dir := filepath.Dir(file); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, nil, err
			}
		}
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, err
		}
		writers = append(writers, f)
		closer = f.Close
	}

	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{Level: parseLevel(level)})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, closer, nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
