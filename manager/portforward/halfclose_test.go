package portforward

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// 客户端发完请求后半关闭写端，仍应收到 VM 在此之后才写出的完整响应。
func TestTCPHalfCloseStillDeliversResponse(t *testing.T) {
	m, _, _ := socketManager(t, "127.0.0.1")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, _ := io.ReadAll(c) // 读到客户端 EOF 后才回响应
		time.Sleep(100 * time.Millisecond)
		_, _ = c.Write(append([]byte("reply:"), req...))
	}()
	guest := ln.Addr().(*net.TCPAddr).Port
	host := availablePort(t, "tcp")
	if err := m.AddMapping(context.Background(), "test", "tcp", host, guest, "", BindV4); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(host)), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("ping"))
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := io.ReadAll(c)
	if string(got) != "reply:ping" {
		t.Fatalf("半关闭后响应被截断：got %q", got)
	}
}
