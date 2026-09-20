package config

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

func TestDeleteConversationIsIdempotentOnV3(t *testing.T) {
	status, body := http.StatusBadRequest, `{"msg":"db: not found","status":400}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conversations/delete" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	// 会话已不存在：两条入口都按成功处理
	if err := ctx.IMDeleteConversation(DeleteConversationReq{UID: "u1", ChannelID: "g1", ChannelType: 2}); err != nil {
		t.Fatalf("IMDeleteConversation() error = %v", err)
	}
	if err := ctx.V3DeleteConversation("u1", "g1", 2); err != nil {
		t.Fatalf("V3DeleteConversation() error = %v", err)
	}
	// 其他错误照常上抛
	body = `{"msg":"internal: slot unavailable","status":400}`
	if err := ctx.IMDeleteConversation(DeleteConversationReq{UID: "u1", ChannelID: "g1", ChannelType: 2}); err == nil {
		t.Fatal("其他错误不该被吞掉")
	}
	if err := ctx.V3DeleteConversation("u1", "g1", 2); err == nil {
		t.Fatal("其他错误不该被吞掉")
	}
}
