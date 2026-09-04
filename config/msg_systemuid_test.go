package config

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/common"
	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

// IM 重启后受信名单只在内存里失效（持久层照常查得到），系统提示会被 reason=3 整类拒收。
// 发送路径必须就地重注册再发一次，否则这段窗口里的进群提示、建群提示全部永久丢失。
func TestSendMessageRetriesAfterSystemUIDCacheLost(t *testing.T) {
	var (
		mu        sync.Mutex
		sendCount int
		calls     []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user/systemuids_add_to_cache":
			_, _ = w.Write([]byte(`{}`))
		case "/message/send":
			sendCount++
			if sendCount == 1 {
				_, _ = w.Write([]byte(`{"message_id":0,"message_seq":0,"reason":3}`))
				return
			}
			_, _ = w.Write([]byte(`{"message_id":9,"message_seq":5,"reason":1}`))
		default:
			t.Errorf("未预期的请求 %s", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	resp, err := ctx.SendMessageWithResult(&MsgSendReq{
		ChannelID:   "g_1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte("tip"),
	})
	if err != nil {
		t.Fatalf("重注册后重发应成功，实际 err = %v", err)
	}
	if resp == nil || resp.MessageID != 9 {
		t.Fatalf("resp = %+v, want message_id=9", resp)
	}
	want := []string{"/message/send", "/user/systemuids_add_to_cache", "/message/send"}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != len(want) {
		t.Fatalf("调用序列 = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("调用序列 = %v, want %v", calls, want)
		}
	}
}

// 非系统账号被拒是真的没权限，不该触发重注册，更不该重发。
func TestSendMessageDoesNotRetryForNonSystemSender(t *testing.T) {
	var (
		mu    sync.Mutex
		calls []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":0,"message_seq":0,"reason":3}`))
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	if _, err := ctx.SendMessageWithResult(&MsgSendReq{
		FromUID:     "u_someone",
		ChannelID:   "g_1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte("hi"),
	}); err == nil {
		t.Fatal("普通用户被拒收应返回错误")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != "/message/send" {
		t.Fatalf("调用序列 = %v, want 只有一次 /message/send", calls)
	}
}

// 重注册带 5 秒限流，避免 IM 真的挂了时把拒收放大成重注册风暴。
func TestRefreshSystemUIDCacheIsRateLimited(t *testing.T) {
	var (
		mu    sync.Mutex
		count int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.Account.SystemUID = "u_10000"
	cfg.Account.FileHelperUID = "fileHelper"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	if !ctx.refreshSystemUIDCache() {
		t.Fatal("首次重注册应成功")
	}
	if ctx.refreshSystemUIDCache() {
		t.Fatal("5 秒内的第二次重注册应被限流跳过")
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("IM 收到 %d 次重注册请求, want 1", count)
	}
}
