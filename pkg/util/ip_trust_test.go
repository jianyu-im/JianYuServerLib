package util

import (
	"net/http/httptest"
	"testing"
)

func TestGetClientPublicIPTrust(t *testing.T) {
	cases := []struct {
		name, remote, xff, want string
	}{
		{"直连客户端伪造 XFF 被忽略", "203.0.113.5:4321", "198.51.100.1", "203.0.113.5"},
		{"可信代理：取真实客户端，左侧伪造无效", "103.239.186.19:5555", "198.51.100.1, 203.0.113.9", "203.0.113.9"},
		{"多级可信代理：跳过代理取最右非代理", "127.0.0.1:1", "203.0.113.9, 210.16.163.9", "203.0.113.9"},
		{"可信代理但无 XFF：退回来源", "210.16.163.9:80", "", "210.16.163.9"},
		{"XFF 含非法值被跳过", "127.0.0.1:1", "203.0.113.9, not-an-ip", "203.0.113.9"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := GetClientPublicIP(r); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}
