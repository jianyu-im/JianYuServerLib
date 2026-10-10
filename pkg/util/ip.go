package util

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// GetExternalIP 获取本服务器的外网IP
func GetExternalIP() (string, error) {
	resp, err := http.Get("https://ipw.cn/api/ip/myip")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	resultBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(resultBytes)), nil
}

// defaultTrustedProxies 自家 HTTP 反向代理：本机、.169（1Panel openresty / 后台前端容器出口）、高防 210.16.163.9。
// 只有 TCP 来源是这些地址时才采信 X-Forwarded-For；四层代理（47.111、103.239.185.115 等）不会加转发头，不在此列。
var defaultTrustedProxies = []string{"127.0.0.1", "::1", "192.140.173.169", "210.16.163.9"}

var (
	trustedOnce sync.Once
	trustedList []string
	trustedNets []*net.IPNet
)

// TrustedProxies 可信代理列表，环境变量 JY_TRUSTED_PROXIES（逗号分隔 IP 或 CIDR）可整体覆盖默认值
func TrustedProxies() []string {
	trustedOnce.Do(func() {
		trustedList = defaultTrustedProxies
		if v := strings.TrimSpace(os.Getenv("JY_TRUSTED_PROXIES")); v != "" {
			trustedList = nil
			for _, item := range strings.Split(v, ",") {
				if item = strings.TrimSpace(item); item != "" {
					trustedList = append(trustedList, item)
				}
			}
		}
		for _, item := range trustedList {
			cidr := item
			if !strings.Contains(cidr, "/") {
				if strings.Contains(cidr, ":") {
					cidr += "/128"
				} else {
					cidr += "/32"
				}
			}
			if _, n, err := net.ParseCIDR(cidr); err == nil {
				trustedNets = append(trustedNets, n)
			}
		}
	})
	return trustedList
}

// IsTrustedProxy ip 是否为可信代理
func IsTrustedProxy(ip string) bool {
	TrustedProxies()
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return false
	}
	for _, n := range trustedNets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// GetClientPublicIP 获取客户端真实 IP。
// 只有 TCP 来源是可信代理时才看转发头，并从 X-Forwarded-For 右往左取第一个非可信代理地址：
// 最左边的值是客户端自己能填的，以前直接取最左边，任何人加个请求头就能伪造登录记录里的 IP（2026-10-10 排查发现）。
func GetClientPublicIP(r *http.Request) string {
	remote := strings.TrimSpace(r.RemoteAddr)
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	if !IsTrustedProxy(remote) {
		return remote
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(hops[i])
		if hop != "" && net.ParseIP(hop) != nil && !IsTrustedProxy(hop) {
			return hop
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); ip != "" && net.ParseIP(ip) != nil {
		return ip
	}
	return remote
}

// GetIPAddress 通过IP获取地址
func GetIPAddress(ip string) (province string, city string, err error) {
	var resp *http.Response
	resp, err = http.Get(fmt.Sprintf("https://restapi.amap.com/v3/ip?key=7e30415c3e9ce73d93d20189b9539be8&ip=%s", ip))
	if err != nil {
		return
	}
	if resp.StatusCode != http.StatusOK {
		err = errors.New("查询地址失败！")
		return
	}
	var data []byte
	data, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var resultMap map[string]interface{}
	resultMap, err = JsonToMap(string(data))
	if err != nil {
		return
	}
	provinceObj := resultMap["province"]
	cityObj := resultMap["city"]
	if provinceObj != nil && cityObj != nil {
		var ok bool
		province, ok = provinceObj.(string)
		if !ok {
			return
		}
		city, ok = cityObj.(string)
		if !ok {
			return
		}
		return
	}
	return
}

// GetIntranetIP 获取本机IP
func GetIntranetIP() (ips []string, err error) {
	ips = make([]string, 0)

	ifaces, e := net.Interfaces()
	if e != nil {
		return ips, e
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}

		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}

		// ignore docker and warden bridge
		if strings.HasPrefix(iface.Name, "docker") || strings.HasPrefix(iface.Name, "w-") {
			continue
		}

		addrs, e := iface.Addrs()
		if e != nil {
			return ips, e
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}

			ipStr := ip.String()
			if IsIntranet(ipStr) {
				ips = append(ips, ipStr)
			}
		}
	}

	return ips, nil
}

// IsIntranet IsIntranet
func IsIntranet(ipStr string) bool {
	if strings.HasPrefix(ipStr, "10.") || strings.HasPrefix(ipStr, "192.168.") {
		return true
	}

	if strings.HasPrefix(ipStr, "172.") {
		// 172.16.0.0-172.31.255.255
		arr := strings.Split(ipStr, ".")
		if len(arr) != 4 {
			return false
		}

		second, err := strconv.ParseInt(arr[1], 10, 64)
		if err != nil {
			return false
		}

		if second >= 16 && second <= 31 {
			return true
		}
	}

	return false
}
