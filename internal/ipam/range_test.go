// 本文件覆盖地址池边界、gateway/保留地址排除、IPv4 最大值终止和非法 range。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package ipam_test

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/cloudnet/cloudnet/internal/ipam"
)

func TestRangeFirstLastAndExclusions(t *testing.T) {
	t.Parallel()

	rng := mustRange(t, "10.77.0.0/29", "10.77.0.1", "10.77.0.0", "10.77.0.7")
	got, err := rng.NextAvailable(nil)
	if err != nil {
		t.Fatalf("NextAvailable() error = %v", err)
	}
	if want := netip.MustParseAddr("10.77.0.2"); got != want {
		t.Fatalf("first address = %s, want %s", got, want)
	}

	used := map[netip.Addr]struct{}{}
	for _, raw := range []string{"10.77.0.2", "10.77.0.3", "10.77.0.4", "10.77.0.5"} {
		used[netip.MustParseAddr(raw)] = struct{}{}
	}
	got, err = rng.NextAvailable(used)
	if err != nil {
		t.Fatalf("NextAvailable() error = %v", err)
	}
	if want := netip.MustParseAddr("10.77.0.6"); got != want {
		t.Fatalf("last address = %s, want %s", got, want)
	}

	used[got] = struct{}{}
	_, err = rng.NextAvailable(used)
	if !errors.Is(err, ipam.ErrExhausted) {
		t.Fatalf("NextAvailable() error = %v, want ErrExhausted", err)
	}
}

func TestRangeSkipsGatewayInsideAllocationRange(t *testing.T) {
	t.Parallel()

	rng := mustRange(t, "10.77.0.0/24", "10.77.0.10", "10.77.0.10", "10.77.0.12")
	got, err := rng.NextAvailable(nil)
	if err != nil {
		t.Fatalf("NextAvailable() error = %v", err)
	}
	if want := netip.MustParseAddr("10.77.0.11"); got != want {
		t.Fatalf("address = %s, want %s", got, want)
	}
}

func TestRangeAtIPv4MaximumTerminates(t *testing.T) {
	t.Parallel()

	rng := mustRange(t, "0.0.0.0/0", "0.0.0.1", "255.255.255.255", "255.255.255.255")
	done := make(chan error, 1)
	go func() {
		_, err := rng.NextAvailable(nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ipam.ErrExhausted) {
			t.Fatalf("NextAvailable() error = %v, want ErrExhausted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NextAvailable() did not terminate at the maximum IPv4 address")
	}
}

func TestRangeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		subnet, gateway      string
		rangeStart, rangeEnd string
	}{
		{name: "ipv6", subnet: "fd00::/64", gateway: "fd00::1", rangeStart: "fd00::2", rangeEnd: "fd00::10"},
		{name: "non canonical subnet", subnet: "10.77.0.1/24", gateway: "10.77.0.1", rangeStart: "10.77.0.10", rangeEnd: "10.77.0.20"},
		{name: "gateway outside", subnet: "10.77.0.0/24", gateway: "10.78.0.1", rangeStart: "10.77.0.10", rangeEnd: "10.77.0.20"},
		{name: "start outside", subnet: "10.77.0.0/24", gateway: "10.77.0.1", rangeStart: "10.78.0.10", rangeEnd: "10.77.0.20"},
		{name: "end outside", subnet: "10.77.0.0/24", gateway: "10.77.0.1", rangeStart: "10.77.0.10", rangeEnd: "10.78.0.20"},
		{name: "reversed range", subnet: "10.77.0.0/24", gateway: "10.77.0.1", rangeStart: "10.77.0.20", rangeEnd: "10.77.0.10"},
		{name: "point to point prefix", subnet: "10.77.0.0/31", gateway: "10.77.0.1", rangeStart: "10.77.0.0", rangeEnd: "10.77.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ipam.NewRange(
				netip.MustParsePrefix(tt.subnet),
				netip.MustParseAddr(tt.gateway),
				netip.MustParseAddr(tt.rangeStart),
				netip.MustParseAddr(tt.rangeEnd),
			)
			if !errors.Is(err, ipam.ErrInvalidRange) {
				t.Fatalf("NewRange() error = %v, want ErrInvalidRange", err)
			}
		})
	}
}

func mustRange(t *testing.T, subnet, gateway, start, end string) ipam.Range {
	t.Helper()
	rng, err := ipam.NewRange(
		netip.MustParsePrefix(subnet),
		netip.MustParseAddr(gateway),
		netip.MustParseAddr(start),
		netip.MustParseAddr(end),
	)
	if err != nil {
		t.Fatalf("NewRange() error = %v", err)
	}
	return rng
}
