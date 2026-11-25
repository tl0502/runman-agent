//go:build containers_image_openpgp

package podman

import (
	"errors"
	"fmt"
	"testing"
)

// storage 库的原始错误会被 bindings 包一层 "creating container storage: "，
// 分类必须看子串而不是全等，否则自愈重试永远不触发。
func TestIsUsernsPoolExhausted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"wrapped", fmt.Errorf("creating container storage: %w", errors.New(usernsPoolExhaustedMsg)), true},
		{"plain", errors.New(usernsPoolExhaustedMsg), true},
		{"other", errors.New("no such container"), false},
	}
	for _, c := range cases {
		if got := isUsernsPoolExhausted(c.err); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// 池容量按同一用户的多条区间累加；注释、空行、别的用户都不算。
func TestSubidCapacity(t *testing.T) {
	data := `# comment
containers:100000:65536
containers:1000000:6553600
root:0:65536

admin:200000:65536
`
	got := subidCapacity(data, "containers")
	want := uint64(65536 + 6553600)
	if got != want {
		t.Fatalf("capacity=%d want %d", got, want)
	}
}
