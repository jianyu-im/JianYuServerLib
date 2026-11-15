package wkhttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type recRenewer struct{ calls []string }

func (r *recRenewer) Renew(token, uid string) { r.calls = append(r.calls, token+"|"+uid) }

// 鉴权通过的 App 用户 token 会触发续期；后台管理员 token、无效 token 不续（2026-11-15 滑动过期）
func TestAuthMiddlewareRenewsOnlyUserTokens(t *testing.T) {
	cache := memCache{
		"token:app": "aa2026codebbb-|@Sass员工-|@Sass-|@Sassaa2026",
		"token:adm": "aa2026codeaaa-|@Sass管理员-|@Sasscompany_admin-|@Sassaa2026",
		"token:old": "aa2026codeccc-|@Sass老后台-|@SasssuperAdmin-|@Sassaa2026",
	}
	l := New()
	rn := &recRenewer{}
	l.Group("/x", l.AuthMiddlewareWithRenewer(cache, "token:", rn)).GET("", func(c *Context) { c.String(200, "ok") })
	for _, tc := range []struct {
		token string
		code  int
	}{{"app", 200}, {"adm", 200}, {"old", 200}, {"gone", 401}} {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("token", tc.token)
		w := httptest.NewRecorder()
		l.ServeHTTP(w, req)
		if w.Code != tc.code {
			t.Fatalf("token=%s code=%d want %d", tc.token, w.Code, tc.code)
		}
	}
	if len(rn.calls) != 1 || rn.calls[0] != "app|aa2026codebbb" {
		t.Fatalf("renew calls = %v, want only app", rn.calls)
	}
}
