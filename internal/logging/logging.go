package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const shortContainerIDLength = 12

type InvocationFields struct {
	Command     string
	Network     string
	ContainerID string
	IfName      string
	NetNS       string
	Bridge      string
	HostVeth    string
	ContainerIP string
}

// New creates a JSON slog logger on the supplied writer. CNI callers pass
// os.Stderr; accepting a writer keeps stdout isolation directly testable.
func New(level string, output io.Writer) (*slog.Logger, error) {
	if output == nil {
		return nil, fmt.Errorf("log output is nil")
	}
	parsed, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{
		Level: parsed,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				attr.Key = "timestamp"
			}
			return attr
		},
	})
	return slog.New(handler), nil
}

func NewStderr(level string) (*slog.Logger, error) {
	return New(level, os.Stderr)
}

func ParseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", level)
	}
}

func WithInvocation(logger *slog.Logger, fields InvocationFields) *slog.Logger {
	return logger.With(
		"command", fields.Command,
		"network", fields.Network,
		"containerID", ShortContainerID(fields.ContainerID),
		"ifName", fields.IfName,
		"netns", fields.NetNS,
		"bridge", fields.Bridge,
		"hostVeth", fields.HostVeth,
		"containerIP", fields.ContainerIP,
	)
}

func ShortContainerID(containerID string) string {
	if len(containerID) <= shortContainerIDLength {
		return containerID
	}
	return containerID[:shortContainerIDLength]
}

// OperationAttrs keeps the dynamic diagnostic schema consistent at command
// exit while allowing callers to attach the original wrapped error.
func OperationAttrs(duration time.Duration, phase string, err error, rollback bool) []slog.Attr {
	return []slog.Attr{
		slog.Duration("duration", duration),
		slog.String("phase", phase),
		slog.Any("error", err),
		slog.Bool("rollback", rollback),
	}
}
