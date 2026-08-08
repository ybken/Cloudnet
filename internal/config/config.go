package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/containernetworking/cni/pkg/types"
)

const (
	NetworkName = "cloudnet-v1"
	PluginType  = "cloudnet"
	BridgeName  = "cni-br0"
	MTU         = 1500

	Subnet     = "10.77.0.0/24"
	Gateway    = "10.77.0.1"
	RangeStart = "10.77.0.10"
	RangeEnd   = "10.77.0.250"

	defaultLogLevel = "info"
	maxConfigBytes  = 1 << 20
)

var ErrInvalidConfig = errors.New("invalid config")

// NetConf is the strict cloudnet V1 network configuration. The CNI common
// fields are retained so CHECK can reconcile prevResult and runtimes can pass
// standard capability data without weakening top-level JSON validation.
type NetConf struct {
	CNIVersion string `json:"cniVersion"`
	Name       string `json:"name"`
	Type       string `json:"type"`

	Args          map[string]json.RawMessage `json:"args,omitempty"`
	Capabilities  map[string]bool            `json:"capabilities,omitempty"`
	RuntimeConfig map[string]json.RawMessage `json:"runtimeConfig,omitempty"`
	DNS           types.DNS                  `json:"dns,omitempty"`

	RawPrevResult    map[string]interface{} `json:"prevResult,omitempty"`
	PrevResult       types.Result           `json:"-"`
	ValidAttachments []types.GCAttachment   `json:"cni.dev/valid-attachments,omitempty"`

	Bridge string     `json:"bridge"`
	MTU    int        `json:"mtu"`
	IPAM   IPAMConfig `json:"ipam"`
	Log    LogConfig  `json:"log,omitempty"`
}

type IPAMConfig struct {
	Subnet     string `json:"subnet"`
	Gateway    string `json:"gateway"`
	RangeStart string `json:"rangeStart"`
	RangeEnd   string `json:"rangeEnd"`

	SubnetPrefix   netip.Prefix `json:"-"`
	GatewayAddr    netip.Addr   `json:"-"`
	RangeStartAddr netip.Addr   `json:"-"`
	RangeEndAddr   netip.Addr   `json:"-"`
}

type LogConfig struct {
	Level string `json:"level,omitempty"`
}

// Parse decodes exactly one JSON object, rejects duplicate and unknown fields,
// and returns a normalized, validated V1 configuration.
func Parse(data []byte) (*NetConf, error) {
	if len(data) == 0 {
		return nil, invalidf("stdin is empty")
	}
	if len(data) > maxConfigBytes {
		return nil, invalidf("stdin exceeds %d bytes", maxConfigBytes)
	}
	if err := checkUniqueJSON(data); err != nil {
		return nil, invalidf("malformed JSON: %v", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var conf NetConf
	if err := decoder.Decode(&conf); err != nil {
		return nil, invalidf("decode JSON: %v", err)
	}
	if err := requireEOF(decoder); err != nil {
		return nil, invalidf("decode JSON: %v", err)
	}
	if err := conf.Validate(); err != nil {
		return nil, err
	}
	return &conf, nil
}

// Load is an explicit alias for Parse used by command handlers.
func Load(data []byte) (*NetConf, error) {
	return Parse(data)
}

func invalidf(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
}

func requireEOF(decoder *json.Decoder) error {
	var extra interface{}
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

// checkUniqueJSON walks the token stream before typed decoding because the
// standard encoding/json decoder otherwise silently accepts duplicate keys.
func checkUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(decoder); err != nil {
		return err
	}
	return requireTokenEOF(decoder)
}

func checkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("expected object end")
		}
	case '[':
		for decoder.More() {
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("expected array end")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
	return nil
}

func requireTokenEOF(decoder *json.Decoder) error {
	_, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func normalizeLogLevel(level string) (string, error) {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return defaultLogLevel, nil
	}
	switch level {
	case "debug", "info", "warn", "error":
		return level, nil
	default:
		return "", fmt.Errorf("unsupported log level %q", level)
	}
}
