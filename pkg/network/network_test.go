package network

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPostForWWWFormEncodesValues 表单值必须做百分号编码后再发送。
//
// 回归自 2026-08-25：原实现手工拼 "k=v&" 不编码，值里只要有一个 '%'（消息正文里的
// 涨跌幅 "1.55%" 最常见），接收端按 urlencoded 解码整个表单就会失败——小米返回
// 65011 "Title or Description is empty"、OPPO 返回 41 "Invalid Arguments"，
// 每天丢掉约 2.3 万条离线推送。
func TestPostForWWWFormEncodesValues(t *testing.T) {
	cases := map[string]string{
		"含百分号":  "张三：今天中位数1.55%，注意",
		"含与号":   "张三：A&B=C",
		"含中文空格": "赵志远 实战粉丝6群",
		"含换行":   "第一行\n第二行",
		"含加号":   "1+1",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			var parseErr error
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				parseErr = r.ParseForm()
				got = r.PostFormValue("description")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":"ok"}`))
			}))
			defer srv.Close()

			_, err := PostForWWWForm(srv.URL, map[string]string{
				"title":       "群标题",
				"description": want,
			}, nil)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			if parseErr != nil {
				t.Fatalf("接收端解析表单失败: %v", parseErr)
			}
			if got != want {
				t.Errorf("description 传输后不一致\n want %q\n  got %q", want, got)
			}
		})
	}
}
