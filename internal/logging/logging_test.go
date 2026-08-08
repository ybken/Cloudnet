package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestNewWritesJSONAndFiltersByLevel(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger, err := New("warn", &output)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger.Info("hidden")
	logger.Warn("visible", "phase", "validate")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("log output is not one JSON record: %q: %v", output.String(), err)
	}
	if got, want := record["level"], "WARN"; got != want {
		t.Errorf("level = %v, want %v", got, want)
	}
	if got, want := record["msg"], "visible"; got != want {
		t.Errorf("msg = %v, want %v", got, want)
	}
	if _, ok := record["timestamp"]; !ok {
		t.Error("JSON record has no timestamp")
	}
}

func TestNewParsesLevelsAndDefaultsToInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  slog.Level
	}{
		{input: "", want: slog.LevelInfo},
		{input: "debug", want: slog.LevelDebug},
		{input: "INFO", want: slog.LevelInfo},
		{input: "warn", want: slog.LevelWarn},
		{input: "error", want: slog.LevelError},
	}
	for _, tc := range tests {
		var output bytes.Buffer
		logger, err := New(tc.input, &output)
		if err != nil {
			t.Errorf("New(%q) error = %v", tc.input, err)
			continue
		}
		if !logger.Enabled(t.Context(), tc.want) {
			t.Errorf("New(%q) did not enable %s", tc.input, tc.want)
		}
	}

	if _, err := New("trace", &bytes.Buffer{}); err == nil {
		t.Fatal("New(trace) succeeded, want error")
	}
	if _, err := New("info", nil); err == nil {
		t.Fatal("New(info, nil) succeeded, want error")
	}
}

func TestWithInvocationAddsBoundedDiagnosticFields(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	base, err := New("info", &output)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger := WithInvocation(base, InvocationFields{
		Command:     "ADD",
		Network:     "cloudnet-v1",
		ContainerID: "0123456789abcdef0123456789abcdef",
		IfName:      "eth0",
		NetNS:       "/var/run/netns/cloudnet-test-1",
		Bridge:      "cni-br0",
		HostVeth:    "cn1234567890123",
		ContainerIP: "10.77.0.10",
	})
	logger.Error("operation failed",
		"duration", 25*time.Millisecond,
		"phase", "configure-route",
		"error", errors.New("route exists"),
		"rollback", true,
	)

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("invalid JSON log: %v", err)
	}
	for key, want := range map[string]any{
		"command": "ADD", "network": "cloudnet-v1", "containerID": "0123456789ab",
		"ifName": "eth0", "netns": "/var/run/netns/cloudnet-test-1", "bridge": "cni-br0",
		"hostVeth": "cn1234567890123", "containerIP": "10.77.0.10",
		"phase": "configure-route", "error": "route exists", "rollback": true,
	} {
		if got := record[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if got := record["duration"]; got != float64(25*time.Millisecond) {
		t.Errorf("duration = %#v, want %d nanoseconds", got, 25*time.Millisecond)
	}
}

func TestShortContainerID(t *testing.T) {
	t.Parallel()

	if got := ShortContainerID("short"); got != "short" {
		t.Errorf("ShortContainerID(short) = %q", got)
	}
	if got := ShortContainerID("0123456789abcdef"); got != "0123456789ab" {
		t.Errorf("ShortContainerID(long) = %q", got)
	}
}
