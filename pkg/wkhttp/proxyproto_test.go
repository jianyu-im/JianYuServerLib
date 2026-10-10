package wkhttp

import (
	"bufio"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jianyu-im/JianYuServerLib/pkg/util"
)

// startProxyProtoServer 起一个套了 PROXY 解析的真实 TCP 服务，isPeer 决定本机来源是否算四层代理
func startProxyProtoServer(t *testing.T, isPeer bool) (addr string, got chan [2]string) {
	t.Helper()
	l := New()
	got = make(chan [2]string, 1)
	l.r.GET("/ip", func(c *gin.Context) {
		got <- [2]string{c.ClientIP(), util.GetClientPublicIP(c.Request)}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: l.r}
	go srv.Serve(wrapProxyProtocol(ln, func(string) bool { return isPeer }))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), got
}

// rawRequest 发原始字节，返回状态行
func rawRequest(t *testing.T, addr, payload string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSpace(line)
}

const getIP = "GET /ip HTTP/1.1\r\nHost: x\r\nX-Forwarded-For: 198.51.100.1\r\nConnection: close\r\n\r\n"

func TestProxyProtocolFromPeer(t *testing.T) {
	addr, got := startProxyProtoServer(t, true)
	// 四层代理带头：取头里的客户端，客户端自填的 XFF 无效
	status := rawRequest(t, addr, "PROXY TCP4 203.0.113.7 10.0.0.1 40000 8000\r\n"+getIP)
	if !strings.Contains(status, "200") {
		t.Fatalf("带头请求失败: %s", status)
	}
	if ips := <-got; ips != [2]string{"203.0.113.7", "203.0.113.7"} {
		t.Fatalf("got %v want 203.0.113.7", ips)
	}
}

func TestProxyProtocolPeerWithoutHeader(t *testing.T) {
	addr, got := startProxyProtoServer(t, true)
	// 代理还没开 proxy_protocol（灰度期）或 .169 七层反代：照旧按来源判断，127.0.0.1 是可信 HTTP 代理所以采信 XFF
	status := rawRequest(t, addr, getIP)
	if !strings.Contains(status, "200") {
		t.Fatalf("无头请求失败: %s", status)
	}
	if ips := <-got; ips != [2]string{"198.51.100.1", "198.51.100.1"} {
		t.Fatalf("got %v want 198.51.100.1", ips)
	}
}

func TestProxyProtocolFromNonPeerRejected(t *testing.T) {
	addr, got := startProxyProtoServer(t, false)
	// 直连客户端自己发 PROXY 头：不解析，当 HTTP 处理直接 400，业务拿不到伪造地址
	status := rawRequest(t, addr, "PROXY TCP4 203.0.113.7 10.0.0.1 40000 8000\r\n"+getIP)
	if !strings.Contains(status, "400") {
		t.Fatalf("非代理来源的 PROXY 头应被拒绝, got %s", status)
	}
	select {
	case ips := <-got:
		t.Fatalf("不应进入业务处理, got %v", ips)
	default:
	}
}

func TestProxyProtocolDoubleHeaderRejected(t *testing.T) {
	addr, got := startProxyProtoServer(t, true)
	// 经代理进来的客户端再夹带一个 PROXY 头：只认第一个（代理加的），第二个当 HTTP 处理 400
	status := rawRequest(t, addr, "PROXY TCP4 203.0.113.7 10.0.0.1 40000 8000\r\nPROXY TCP4 1.2.3.4 10.0.0.1 1 8000\r\n"+getIP)
	if !strings.Contains(status, "400") {
		t.Fatalf("夹带的第二个 PROXY 头应被拒绝, got %s", status)
	}
	select {
	case ips := <-got:
		t.Fatalf("不应进入业务处理, got %v", ips)
	default:
	}
}
