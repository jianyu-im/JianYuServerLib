package wkhttp

import (
	"net"
	"os"
	"time"

	"github.com/jianyu-im/JianYuServerLib/pkg/util"
	proxyproto "github.com/pires/go-proxyproto"
)

// proxyHeaderTimeout 来自四层代理的连接等待 PROXY 头的上限，超时按「无头」处理（客户端都是先发数据，正常不会等到）
const proxyHeaderTimeout = 5 * time.Second

// proxyProtocolPolicy 只对四层代理来源解析 PROXY 头，其余来源原样透传。
// 代理来源用 USE（有头用头、无头按普通连接）：灰度期间代理逐台开 proxy_protocol 也不会断流；
// .169 上 1Panel 七层反代（不带头）和 game-proxy 四层（带头）共用出口 IP，也只能用 USE。
// 决策函数绝不能返回 error：Accept 收到 error 会让整个 http.Serve 退出。
func proxyProtocolPolicy(isPeer func(ip string) bool) proxyproto.PolicyFunc {
	return func(upstream net.Addr) (proxyproto.Policy, error) {
		host, _, err := net.SplitHostPort(upstream.String())
		if err != nil || !isPeer(host) {
			return proxyproto.SKIP, nil
		}
		return proxyproto.USE, nil
	}
}

// wrapProxyProtocol 给监听器套上 PROXY protocol 解析，解析后 Request.RemoteAddr 即真实客户端
func wrapProxyProtocol(ln net.Listener, isPeer func(ip string) bool) net.Listener {
	return &proxyproto.Listener{
		Listener:          ln,
		Policy:            proxyProtocolPolicy(isPeer),
		ReadHeaderTimeout: proxyHeaderTimeout,
	}
}

// resolveAddress 与 gin.Run 的地址规则一致：不传取 PORT 环境变量，再缺省 :8080
func resolveAddress(addr []string) string {
	if len(addr) > 0 {
		return addr[0]
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

// runWithProxyProtocol 替代 gin.Run：自己监听再套 PROXY protocol，其余行为与 gin.Run 相同
func (l *WKHttp) runWithProxyProtocol(addr ...string) error {
	ln, err := net.Listen("tcp", resolveAddress(addr))
	if err != nil {
		return err
	}
	return l.r.RunListener(wrapProxyProtocol(ln, util.IsProxyProtocolPeer))
}
