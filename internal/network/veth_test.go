// 本文件验证 netlink setter 后必须刷新内核快照，并拒绝无法证明 alias 的 veth。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package network

import (
	"errors"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestReloadOwnedVethWithUsesFreshKernelSnapshot(t *testing.T) {
	t.Parallel()

	const (
		name  = "cn0123456789abc"
		alias = "cloudnet:v1:cloudnet-v1:0123456789abcdef"
	)
	staleAttrs := netlink.NewLinkAttrs()
	staleAttrs.Name = name
	stale := &netlink.Veth{LinkAttrs: staleAttrs}
	freshAttrs := staleAttrs
	freshAttrs.Alias = alias
	fresh := &netlink.Veth{LinkAttrs: freshAttrs}
	lookupCalls := 0

	got, err := reloadOwnedVethWith(name, alias, func(gotName string) (netlink.Link, bool, error) {
		lookupCalls++
		if gotName != name {
			t.Fatalf("lookup name = %q, want %q", gotName, name)
		}
		return fresh, true, nil
	})
	if err != nil {
		t.Fatalf("reloadOwnedVethWith() error = %v", err)
	}
	if lookupCalls != 1 {
		t.Fatalf("lookup calls = %d, want 1", lookupCalls)
	}
	if got != fresh {
		t.Fatalf("returned link = %p, want fresh kernel snapshot %p", got, fresh)
	}
	if stale.Attrs().Alias != "" {
		t.Fatalf("stale snapshot alias unexpectedly changed to %q", stale.Attrs().Alias)
	}
}

func TestReloadOwnedVethWithRejectsUnverifiableKernelState(t *testing.T) {
	t.Parallel()

	const (
		name  = "cn0123456789abc"
		alias = "cloudnet:v1:cloudnet-v1:0123456789abcdef"
	)
	lookupErr := errors.New("injected lookup failure")

	tests := []struct {
		name       string
		lookup     linkLookupFunc
		wantError  error
		wantString string
	}{
		{
			name: "lookup error",
			lookup: func(string) (netlink.Link, bool, error) {
				return nil, false, lookupErr
			},
			wantError: lookupErr,
		},
		{
			name: "missing after update",
			lookup: func(string) (netlink.Link, bool, error) {
				return nil, false, nil
			},
			wantString: "missing after kernel update",
		},
		{
			name: "alias mismatch",
			lookup: func(string) (netlink.Link, bool, error) {
				attrs := netlink.NewLinkAttrs()
				attrs.Name = name
				attrs.Alias = "wrong"
				return &netlink.Veth{LinkAttrs: attrs}, true, nil
			},
			wantString: "ownership mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := reloadOwnedVethWith(name, alias, tc.lookup)
			if err == nil {
				t.Fatal("reloadOwnedVethWith() unexpectedly succeeded")
			}
			if tc.wantError != nil && !errors.Is(err, tc.wantError) {
				t.Fatalf("error = %v, want wrapped %v", err, tc.wantError)
			}
			if tc.wantString != "" && !strings.Contains(err.Error(), tc.wantString) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantString)
			}
		})
	}
}
