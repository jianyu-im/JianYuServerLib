package wkhttp

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gin 访问日志用的 ClientIP 与登录记录口径一致：只采信可信代理的转发头
func TestClientIPTrustedProxies(t *testing.T) {
	l := New()
	var got string
	l.r.GET("/ip", func(c *gin.Context) { got = c.ClientIP() })
	for _, tc := range []struct{ remote, xff, want string }{
		{"203.0.113.5:1", "198.51.100.1", "203.0.113.5"},
		{"192.140.173.169:1", "198.51.100.1, 203.0.113.9", "203.0.113.9"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/ip", nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", tc.xff)
		l.r.ServeHTTP(httptest.NewRecorder(), req)
		if got != tc.want {
			t.Fatalf("remote=%s xff=%s: got %s want %s", tc.remote, tc.xff, got, tc.want)
		}
	}
}
