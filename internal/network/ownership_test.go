package network

import (
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestVerifyOwnedLinkRequiresExactAlias(t *testing.T) {
	expected := "cloudnet:v1:0123456789abcdef"
	tests := []struct {
		name    string
		alias   string
		wantErr bool
	}{
		{name: "exact", alias: expected},
		{name: "empty", alias: "", wantErr: true},
		{name: "prefix only", alias: "cloudnet:v1:", wantErr: true},
		{name: "different endpoint", alias: expected + "0", wantErr: true},
		{name: "leading whitespace", alias: " " + expected, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attrs := netlink.NewLinkAttrs()
			attrs.Name = "cn0123456789ab"
			attrs.Alias = tc.alias
			err := verifyOwnedLink(&attrs, expected)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyOwnedLink() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "ownership") {
				t.Fatalf("verifyOwnedLink() error = %v, want ownership context", err)
			}
		})
	}
}
