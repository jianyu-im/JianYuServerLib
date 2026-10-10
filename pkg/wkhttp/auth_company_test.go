package wkhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// memCache 测试用内存缓存
type memCache map[string]string

func (m memCache) Set(k, v string) error                           { m[k] = v; return nil }
func (m memCache) Delete(k string) error                           { delete(m, k); return nil }
func (m memCache) SetAndExpire(k, v string, _ time.Duration) error { m[k] = v; return nil }
func (m memCache) Get(k string) (string, error)                    { return m[k], nil }

// 后台管理员 token 以 token 内企业号为准，请求头里的 companyCode 不能跨企业；App token 照旧
func TestAuthMiddlewareCompanyCode(t *testing.T) {
	cache := memCache{
		"token:adm": "LY101codeaaa-|@Sass管理员-|@Sasscompany_admin-|@SassLY101",
		"token:app": "LY101codebbb-|@Sass员工-|@Sass-|@SassLY101",
	}
	l := New()
	var got string
	l.Group("/x", l.AuthMiddleware(cache, "token:")).GET("", func(c *Context) { got = c.GetCompanyCode(); c.String(200, "ok") })
	cases := []struct{ token, header, want string }{
		{"adm", "SY068", "LY101"}, // 后台 token 伪造请求头跨企业 → 仍是自己企业
		{"adm", "", "LY101"},
		{"app", "SY068", "SY068"}, // App 端保持原行为
		{"app", "", "LY101"},      // 无请求头按 uid 前缀
	}
	for _, tc := range cases {
		got = ""
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("token", tc.token)
		if tc.header != "" {
			req.Header.Set("companyCode", tc.header)
		}
		w := httptest.NewRecorder()
		l.ServeHTTP(w, req)
		if w.Code != 200 || got != tc.want {
			t.Fatalf("token=%s header=%s: code=%d companyCode=%q, want %q", tc.token, tc.header, w.Code, got, tc.want)
		}
	}
}
