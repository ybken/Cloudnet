// 本文件用表驱动场景验证 prevResult 的接口、IPv4、gateway、default route 规范化及具体 mismatch 诊断。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package cni

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

func TestValidatePrevResult(t *testing.T) {
	t.Parallel()

	expected := ResultData{
		CNIVersion:   "1.1.0",
		NetNS:        "/run/netns/cloudnet-test-a",
		BridgeName:   "cni-br0",
		BridgeMAC:    "02:00:00:00:00:01",
		HostName:     "cn0123456789abc",
		HostMAC:      "02:00:00:00:00:02",
		IfName:       "eth0",
		ContainerMAC: "02:00:00:00:00:03",
		MTU:          1500,
		Address:      netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:      netip.MustParseAddr("10.77.0.1"),
	}
	raw := resultAsMap(t, expected)

	if err := ValidatePrevResult(raw, expected.CNIVersion, expected); err != nil {
		t.Fatalf("ValidatePrevResult() error = %v", err)
	}
}

func TestValidatePrevResultReportsSpecificMismatch(t *testing.T) {
	t.Parallel()

	expected := ResultData{
		CNIVersion: "1.1.0",
		NetNS:      "/run/netns/cloudnet-test-a",
		BridgeName: "cni-br0",
		HostName:   "cn0123456789abc",
		IfName:     "eth0",
		MTU:        1500,
		Address:    netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:    netip.MustParseAddr("10.77.0.1"),
	}

	tests := []struct {
		name    string
		mutate  func(map[string]interface{})
		wantErr string
	}{
		{
			name: "container interface",
			mutate: func(raw map[string]interface{}) {
				raw["interfaces"].([]interface{})[2].(map[string]interface{})["name"] = "eth9"
			},
			wantErr: "container interface",
		},
		{
			name: "address",
			mutate: func(raw map[string]interface{}) {
				raw["ips"].([]interface{})[0].(map[string]interface{})["address"] = "10.77.0.99/24"
			},
			wantErr: "address",
		},
		{
			name: "gateway",
			mutate: func(raw map[string]interface{}) {
				raw["ips"].([]interface{})[0].(map[string]interface{})["gateway"] = "10.77.0.2"
			},
			wantErr: "gateway",
		},
		{
			name: "default route",
			mutate: func(raw map[string]interface{}) {
				raw["routes"] = []interface{}{}
			},
			wantErr: "default route",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := resultAsMap(t, expected)
			tc.mutate(raw)
			err := ValidatePrevResult(raw, expected.CNIVersion, expected)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidatePrevResult() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidatePrevResultRequiresExactlyOneContainerIPv4(t *testing.T) {
	t.Parallel()

	expected := ResultData{
		CNIVersion: "1.1.0",
		NetNS:      "/run/netns/cloudnet-test-a",
		BridgeName: "cni-br0",
		HostName:   "cn0123456789abc",
		IfName:     "eth0",
		MTU:        1500,
		Address:    netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:    netip.MustParseAddr("10.77.0.1"),
	}

	tests := []struct {
		name    string
		mutate  func(map[string]interface{})
		wantErr string
	}{
		{
			name: "duplicate expected address",
			mutate: func(raw map[string]interface{}) {
				ips := raw["ips"].([]interface{})
				raw["ips"] = append(ips, cloneJSONMap(ips[0].(map[string]interface{})))
			},
			wantErr: "IPv4 address count is 2, want 1",
		},
		{
			name: "additional different address",
			mutate: func(raw map[string]interface{}) {
				ips := raw["ips"].([]interface{})
				extra := cloneJSONMap(ips[0].(map[string]interface{}))
				extra["address"] = "10.77.0.99/24"
				raw["ips"] = append(ips, extra)
			},
			wantErr: "IPv4 address count is 2, want 1",
		},
		{
			name: "expected address missing",
			mutate: func(raw map[string]interface{}) {
				raw["ips"] = []interface{}{}
			},
			wantErr: "IPv4 address count is 0, want 1",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw := resultAsMap(t, expected)
			tc.mutate(raw)
			err := ValidatePrevResult(raw, expected.CNIVersion, expected)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidatePrevResult() error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidatePrevResultIgnoresIPv4ForOtherInterface(t *testing.T) {
	t.Parallel()

	expected := ResultData{
		CNIVersion: "1.1.0",
		NetNS:      "/run/netns/cloudnet-test-a",
		BridgeName: "cni-br0",
		HostName:   "cn0123456789abc",
		IfName:     "eth0",
		MTU:        1500,
		Address:    netip.MustParsePrefix("10.77.0.10/24"),
		Gateway:    netip.MustParseAddr("10.77.0.1"),
	}
	raw := resultAsMap(t, expected)
	ips := raw["ips"].([]interface{})
	extra := cloneJSONMap(ips[0].(map[string]interface{}))
	extra["interface"] = float64(1)
	extra["address"] = "192.0.2.10/24"
	extra["gateway"] = "192.0.2.1"
	raw["ips"] = append(ips, extra)

	if err := ValidatePrevResult(raw, expected.CNIVersion, expected); err != nil {
		t.Fatalf("ValidatePrevResult() error = %v, want IP for other interface ignored", err)
	}
}

func TestValidatePrevResultAbsentIsAllowed(t *testing.T) {
	t.Parallel()
	if err := ValidatePrevResult(nil, "1.1.0", ResultData{}); err != nil {
		t.Fatalf("ValidatePrevResult(nil) error = %v", err)
	}
}

func resultAsMap(t *testing.T, data ResultData) map[string]interface{} {
	t.Helper()
	rawJSON, err := json.Marshal(BuildResult(data))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(rawJSON, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func cloneJSONMap(input map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
