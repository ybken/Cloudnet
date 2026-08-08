package config

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
)

const validConfig = `{
  "cniVersion": "1.1.0",
  "name": "cloudnet-v1",
  "type": "cloudnet",
  "bridge": "cni-br0",
  "mtu": 1500,
  "ipam": {
    "subnet": "10.77.0.0/24",
    "gateway": "10.77.0.1",
    "rangeStart": "10.77.0.10",
    "rangeEnd": "10.77.0.250"
  },
  "log": {"level": "info"}
}`

func TestParseValidConfigDerivesIPv4Values(t *testing.T) {
	t.Parallel()

	conf, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if conf.CNIVersion != "1.1.0" || conf.Name != NetworkName || conf.Type != PluginType {
		t.Fatalf("unexpected identity fields: %#v", conf)
	}
	if got, want := conf.IPAM.SubnetPrefix, netip.MustParsePrefix("10.77.0.0/24"); got != want {
		t.Errorf("SubnetPrefix = %s, want %s", got, want)
	}
	if got, want := conf.IPAM.GatewayAddr, netip.MustParseAddr("10.77.0.1"); got != want {
		t.Errorf("GatewayAddr = %s, want %s", got, want)
	}
	if got, want := conf.IPAM.RangeStartAddr, netip.MustParseAddr("10.77.0.10"); got != want {
		t.Errorf("RangeStartAddr = %s, want %s", got, want)
	}
	if got, want := conf.IPAM.RangeEndAddr, netip.MustParseAddr("10.77.0.250"); got != want {
		t.Errorf("RangeEndAddr = %s, want %s", got, want)
	}
}

func TestParseAcceptsSupportedCNICommonFields(t *testing.T) {
	t.Parallel()

	input := strings.TrimSuffix(validConfig, "}") + `,
  "args": {"cni": {"ignoreUnknown": true}},
  "capabilities": {"portMappings": false},
  "runtimeConfig": {"portMappings": []},
  "dns": {"nameservers": ["10.77.0.1"], "search": ["example.test"]},
  "prevResult": {"cniVersion": "1.1.0", "interfaces": []},
  "cni.dev/valid-attachments": [{"containerID": "abc", "ifname": "eth0"}]
}`

	conf, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() rejected CNI common fields: %v", err)
	}
	if conf.RawPrevResult == nil || len(conf.ValidAttachments) != 1 {
		t.Fatalf("common fields were not retained: %#v", conf)
	}
}

func TestParseIsStrictJSON(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unknown top-level field": strings.Replace(validConfig, `"mtu": 1500`, `"mtu": 1500, "surprise": true`, 1),
		"unknown nested field":    strings.Replace(validConfig, `"level": "info"`, `"level": "info", "file": "/tmp/log"`, 1),
		"duplicate field":         strings.Replace(validConfig, `"mtu": 1500`, `"mtu": 1500, "mtu": 1400`, 1),
		"trailing document":       validConfig + `{}`,
		"empty document":          "",
	}

	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(input))
			if err == nil {
				t.Fatal("Parse() succeeded, want error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Parse() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestParseRejectsInvalidV1Config(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unsupported version":  replaceJSON(validConfig, `"cniVersion": "1.1.0"`, `"cniVersion": "0.4.0"`),
		"unsafe network name":  replaceJSON(validConfig, `"name": "cloudnet-v1"`, `"name": "../cloudnet-v1"`),
		"other network name":   replaceJSON(validConfig, `"name": "cloudnet-v1"`, `"name": "cloudnet-v2"`),
		"wrong plugin type":    replaceJSON(validConfig, `"type": "cloudnet"`, `"type": "bridge"`),
		"wrong bridge":         replaceJSON(validConfig, `"bridge": "cni-br0"`, `"bridge": "br0"`),
		"wrong mtu":            replaceJSON(validConfig, `"mtu": 1500`, `"mtu": 1400`),
		"ipv6 subnet":          replaceJSON(validConfig, `"subnet": "10.77.0.0/24"`, `"subnet": "fd77::/64"`),
		"non-canonical subnet": replaceJSON(validConfig, `"subnet": "10.77.0.0/24"`, `"subnet": "10.77.0.5/24"`),
		"wrong subnet":         replaceJSON(validConfig, `"subnet": "10.77.0.0/24"`, `"subnet": "10.78.0.0/24"`),
		"wrong gateway":        replaceJSON(validConfig, `"gateway": "10.77.0.1"`, `"gateway": "10.77.0.2"`),
		"range reversed":       replaceJSON(validConfig, `"rangeStart": "10.77.0.10"`, `"rangeStart": "10.77.0.251"`),
		"range outside":        replaceJSON(validConfig, `"rangeEnd": "10.77.0.250"`, `"rangeEnd": "10.77.1.10"`),
		"gateway in range":     replaceJSON(replaceJSON(validConfig, `"rangeStart": "10.77.0.10"`, `"rangeStart": "10.77.0.1"`), `"rangeEnd": "10.77.0.250"`, `"rangeEnd": "10.77.0.250"`),
		"bad log level":        replaceJSON(validConfig, `"level": "info"`, `"level": "trace"`),
	}

	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(input))
			if err == nil {
				t.Fatal("Parse() succeeded, want error")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Parse() error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestParseSupportsCNI100AndDefaultsLogLevel(t *testing.T) {
	t.Parallel()

	input := replaceJSON(validConfig, `"cniVersion": "1.1.0"`, `"cniVersion": "1.0.0"`)
	input = replaceJSON(input, `,
  "log": {"level": "info"}`, ``)
	conf, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := conf.Log.Level, "info"; got != want {
		t.Errorf("Log.Level = %q, want %q", got, want)
	}
}

func TestValidateNetworkName(t *testing.T) {
	t.Parallel()

	valid := []string{"cloudnet-v1", "a", "net_01.test"}
	for _, name := range valid {
		if err := ValidateNetworkName(name); err != nil {
			t.Errorf("ValidateNetworkName(%q) = %v", name, err)
		}
	}

	invalid := []string{"", ".", "..", "../x", "/absolute", "a/b", " hidden", "-leading", "trailing-", "name with space", strings.Repeat("a", 64)}
	for _, name := range invalid {
		if err := ValidateNetworkName(name); err == nil {
			t.Errorf("ValidateNetworkName(%q) succeeded", name)
		}
	}
}

func TestValidateRuntime(t *testing.T) {
	t.Parallel()

	if err := ValidateRuntime(strings.Repeat("a", 64), "/var/run/netns/cloudnet-test-1", "eth0", true); err != nil {
		t.Fatalf("ValidateRuntime(valid) = %v", err)
	}
	if err := ValidateRuntime("sha256:abcdef", "", "eth0", false); err != nil {
		t.Fatalf("ValidateRuntime(DEL without netns) = %v", err)
	}

	tests := []struct {
		name        string
		containerID string
		netns       string
		ifName      string
		requireNS   bool
	}{
		{name: "empty container", netns: "/var/run/netns/x", ifName: "eth0", requireNS: true},
		{name: "unsafe container", containerID: "a/b", netns: "/var/run/netns/x", ifName: "eth0", requireNS: true},
		{name: "missing required netns", containerID: "abc", ifName: "eth0", requireNS: true},
		{name: "relative netns", containerID: "abc", netns: "var/run/netns/x", ifName: "eth0", requireNS: true},
		{name: "unclean netns", containerID: "abc", netns: "/var/run/../run/netns/x", ifName: "eth0", requireNS: true},
		{name: "empty ifname", containerID: "abc", netns: "/var/run/netns/x", requireNS: true},
		{name: "long ifname", containerID: "abc", netns: "/var/run/netns/x", ifName: "0123456789abcdef", requireNS: true},
		{name: "unsafe ifname", containerID: "abc", netns: "/var/run/netns/x", ifName: "eth/0", requireNS: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateRuntime(tc.containerID, tc.netns, tc.ifName, tc.requireNS); err == nil {
				t.Fatal("ValidateRuntime() succeeded, want error")
			}
		})
	}
}

func replaceJSON(input, old, new string) string {
	return strings.Replace(input, old, new, 1)
}
