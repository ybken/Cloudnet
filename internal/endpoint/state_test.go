// 本文件验证 endpoint key 的稳定无歧义摘要以及记录身份字段校验。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package endpoint_test

import (
	"testing"

	"github.com/cloudnet/cloudnet/internal/endpoint"
)

func TestKeyIDIsStableAndUnambiguous(t *testing.T) {
	t.Parallel()

	key := endpoint.Key{
		NetworkName: "cloudnet-v1",
		ContainerID: "container-a",
		IfName:      "eth0",
	}
	if got, want := key.ID(), key.ID(); got != want {
		t.Fatalf("ID() is not stable: %q != %q", got, want)
	}
	if len(key.ID()) != 64 {
		t.Fatalf("ID() length = %d, want 64", len(key.ID()))
	}

	// Delimiters or concatenation cannot make distinct keys collide.
	other := endpoint.Key{
		NetworkName: "cloudnet-v1",
		ContainerID: "container",
		IfName:      "aeth0",
	}
	if key.ID() == other.ID() {
		t.Fatal("distinct endpoint keys produced the same ID")
	}
}

func TestKeyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  endpoint.Key
	}{
		{name: "missing network", key: endpoint.Key{ContainerID: "c", IfName: "eth0"}},
		{name: "missing container", key: endpoint.Key{NetworkName: "n", IfName: "eth0"}},
		{name: "missing interface", key: endpoint.Key{NetworkName: "n", ContainerID: "c"}},
		{name: "interface too long", key: endpoint.Key{NetworkName: "n", ContainerID: "c", IfName: "1234567890123456"}},
		{name: "interface contains slash", key: endpoint.Key{NetworkName: "n", ContainerID: "c", IfName: "bad/name"}},
		{name: "container contains nul", key: endpoint.Key{NetworkName: "n", ContainerID: "bad\x00id", IfName: "eth0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.key.Validate(); err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
		})
	}

	valid := endpoint.Key{NetworkName: "cloudnet-v1", ContainerID: "abc123", IfName: "eth0"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
