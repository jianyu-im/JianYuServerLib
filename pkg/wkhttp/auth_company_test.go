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

// 后台与 App token 都以 token 内企业号为准，请求头里的 companyCode 不能跨企业（2026-10-21 App 端收紧）
func TestAuthMiddlewareCompanyCode(t *testing.T) {
	cache := memCache{
		"token:adm": "aa2026codeaaa-|@Sass管理员-|@Sasscompany_admin-|@Sassaa2026",
		"token:sup": "1001codeccc-|@Sass超管-|@Sasscompany_admin-|@SassDF8886", // 超管进入企业签发的 token
		"token:app": "aa2026codebbb-|@Sass员工-|@Sass-|@Sassaa2026",
		"token:old": "F3823codeddd-|@Sass老token-|@Sass-|@Sass", // 老 token 没写企业号
		"token:sys": "u_10000-|@Sass小助手-|@Sass-|@Sass",         // 系统账号 uid 不带 code
	}
	l := New()
	var got string
	l.Group("/x", l.AuthMiddleware(cache, "token:")).GET("", func(c *Context) { got = c.GetCompanyCode(); c.String(200, "ok") })
	cases := []struct{ token, header, want string }{
		{"adm", "DF8886", "aa2026"}, // 后台 token 伪造请求头跨企业 → 仍是自己企业
		{"adm", "", "aa2026"},
		{"sup", "1001", "DF8886"},   // 进入企业的 token 以 token 里的企业为准
		{"app", "DF8886", "aa2026"}, // App 端伪造请求头跨企业 → 仍是自己企业（V9 群事件）
		{"app", "AA2026", "aa2026"},
		{"app", "", "aa2026"},
		{"old", "HH8888", "F3823"}, // 老 token 按 uid 前缀，不信请求头
		{"old", "", "F3823"},
		{"sys", "HH8888", "HH8888"}, // 系统账号没有归属企业，沿用请求头
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
