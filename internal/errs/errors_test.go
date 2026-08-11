// 本文件验证分类错误包装保留 cause/context，并保持 nil-in nil-out。
// 测试名描述场景，子测试分别表达输入变化与期望结果。
package errs

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapPreservesCauseAndContext(t *testing.T) {
	t.Parallel()

	cause := errors.New("permission denied")
	err := Wrap(KindStatePersistence, "write endpoint state", cause)

	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause) = false", err)
	}
	if !IsKind(err, KindStatePersistence) {
		t.Fatalf("IsKind(%v, %q) = false", err, KindStatePersistence)
	}
	for _, want := range []string{"state persistence failed", "write endpoint state", "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestWrapNilReturnsNil(t *testing.T) {
	t.Parallel()

	if err := Wrap(KindInvalidConfig, "parse", nil); err != nil {
		t.Fatalf("Wrap(..., nil) = %v, want nil", err)
	}
}
