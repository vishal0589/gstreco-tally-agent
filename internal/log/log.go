// Package log wires zerolog to a lumberjack-rotated file under
// %ProgramData%\GST Reco\agent\logs on Windows (or ~/.gstreco-agent/logs
// elsewhere) and to stderr in dev. All other packages use this Logger so
// format + sink stay consistent.
package log

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Options configures the logger. Zero value gives a reasonable default.
type Options struct {
	// Dir overrides the default log directory. Empty = platform default.
	Dir string
	// Level is one of "debug", "info", "warn", "error". Empty = "info".
	Level string
	// Console adds a human-readable writer to stderr alongside the file sink.
	// Always true outside Windows services.
	Console bool
}

// New returns a zerolog.Logger writing to a rotating file (10 MB × 5 files,
// gzip-compressed, 30-day retention) and optionally stderr. The returned
// Logger includes ISO-8601 timestamps and the caller file:line.
func New(opts Options) zerolog.Logger {
	dir := opts.Dir
	if dir == "" {
		dir = defaultLogDir()
	}
	_ = os.MkdirAll(dir, 0o755)

	fileSink := &lumberjack.Logger{
		Filename:   filepath.Join(dir, "agent.log"),
		MaxSize:    10,
		MaxBackups: 5,
		MaxAge:     30,
		Compress:   true,
	}

	var writer io.Writer = fileSink
	if opts.Console {
		writer = io.MultiWriter(fileSink, zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	}

	zerolog.TimeFieldFormat = time.RFC3339
	level, err := zerolog.ParseLevel(opts.Level)
	if err != nil || opts.Level == "" {
		level = zerolog.InfoLevel
	}

	return zerolog.New(writer).Level(level).With().Timestamp().Caller().Logger()
}

func defaultLogDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "GST Reco", "agent", "logs")
		}
		return `C:\ProgramData\GST Reco\agent\logs`
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "gstreco-agent", "logs")
	}
	return filepath.Join(home, ".gstreco-agent", "logs")
}
