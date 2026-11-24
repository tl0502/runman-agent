package agent

import (
	"testing"

	"google.golang.org/protobuf/proto"
)

// 旧平台不传 bind_family（零值）时，序列化结果不应出现字段 6，
// 且反序列化后仍是 0 —— 保证与旧平台双向兼容。
func TestBindFamilyZeroCompat(t *testing.T) {
	in := &CmdSetPortForward{VmId: "vm-1", HostPort: 2222, GuestPort: 22}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := &CmdSetPortForward{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.BindFamily != 0 {
		t.Fatalf("bind_family = %d, want 0", out.BindFamily)
	}
	if out.HostPort != 2222 || out.GuestPort != 22 || out.VmId != "vm-1" {
		t.Fatalf("other fields corrupted: %+v", out)
	}
}

func TestBindFamilyRoundTrip(t *testing.T) {
	for _, fam := range []int32{0, 1, 2} {
		in := &CmdSetPortForward{VmId: "vm-1", HostPort: 8080, GuestPort: 80, BindFamily: fam}
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatalf("marshal(%d): %v", fam, err)
		}
		out := &CmdSetPortForward{}
		if err := proto.Unmarshal(b, out); err != nil {
			t.Fatalf("unmarshal(%d): %v", fam, err)
		}
		if out.BindFamily != fam {
			t.Fatalf("bind_family = %d, want %d", out.BindFamily, fam)
		}

		entry := &PortForwardEntry{VmId: "vm-1", HostPort: 8080, GuestPort: 80, BindFamily: fam}
		eb, err := proto.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal entry(%d): %v", fam, err)
		}
		eo := &PortForwardEntry{}
		if err := proto.Unmarshal(eb, eo); err != nil {
			t.Fatalf("unmarshal entry(%d): %v", fam, err)
		}
		if eo.BindFamily != fam {
			t.Fatalf("entry bind_family = %d, want %d", eo.BindFamily, fam)
		}
	}
}
