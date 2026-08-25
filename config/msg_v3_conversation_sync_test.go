package config

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

// newConvSyncV3Context 造一个指向指定假 IM 的 v3 Context。
// 注意每个测试要用不同的 uid：会话列表结果按 uid 有 45 秒的进程级复用缓存。
func newConvSyncV3Context(t *testing.T, handler http.HandlerFunc) *Context {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.APIVersion = "v3"
	cfg.Account.SystemUID = "u_10000"
	return &Context{cfg: cfg, Log: log.NewTLog("test")}
}

func TestIMSyncUserConversationV3FreshInstallReconcilesBusinessGroups(t *testing.T) {
	var mu sync.Mutex
	listCalls := 0
	activationCalls := 0
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			mu.Lock()
			listCalls++
			call := listCalls
			mu.Unlock()
			if call == 1 {
				_, _ = w.Write([]byte(`{"conversations":[],"done":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g1","channel_type":2,"active_at":1000000,"unread":1,"last_message":{"message_id":8,"message_idstr":"8","message_seq":8,"from_uid":"u2","client_msg_no":"m8","server_timestamp_ms":1008000,"payload":"e30="}}],"done":true}`))
		case "/conversations/activate":
			var req struct {
				UID         string `json:"uid"`
				ChannelID   string `json:"channel_id"`
				ChannelType uint8  `json:"channel_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode activation: %v", err)
			}
			if req.UID != "sync_u1" || req.ChannelID != "g1" || req.ChannelType != 2 {
				t.Errorf("activation = %+v", req)
			}
			mu.Lock()
			activationCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	})

	got, err := ctx.IMSyncUserConversation("sync_u1", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if activationCalls != 1 || listCalls != 2 {
		t.Fatalf("activationCalls=%d listCalls=%d, want 1/2", activationCalls, listCalls)
	}
	if len(got) != 1 || got[0].ChannelID != "g1" || got[0].LastMsgSeq != 8 || len(got[0].Recents) != 1 {
		t.Fatalf("conversations = %+v, want reconciled g1 with subsequent message", got)
	}
}

func TestIMSyncUserConversationV3RebuildsMissingMembershipWithHistoryPolicy(t *testing.T) {
	var mu sync.Mutex
	listCalls := 0
	activationCalls := 0
	projectionCalls := 0
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			mu.Lock()
			listCalls++
			call := listCalls
			mu.Unlock()
			if call == 1 {
				_, _ = w.Write([]byte(`{"conversations":[],"done":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g1","channel_type":2,"active_at":1000000,"last_message":{"message_id":3,"message_idstr":"3","message_seq":3,"from_uid":"u2","client_msg_no":"m3","server_timestamp_ms":1003000,"payload":"e30="}}],"done":true}`))
		case "/conversations/activate":
			mu.Lock()
			activationCalls++
			call := activationCalls
			mu.Unlock()
			if call == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"msg":"meta: not found"}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case "/channel/subscriber_add":
			var req struct {
				ChannelID      string   `json:"channel_id"`
				ChannelType    uint8    `json:"channel_type"`
				HistoryVisible int      `json:"history_visible"`
				Subscribers    []string `json:"subscribers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode membership projection: %v", err)
			}
			if req.ChannelID != "g1" || req.ChannelType != 2 || req.HistoryVisible != 1 || len(req.Subscribers) != 1 || req.Subscribers[0] != "sync_u2" {
				t.Errorf("membership projection = %+v", req)
			}
			mu.Lock()
			projectionCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	})

	got, err := ctx.IMSyncUserConversation("sync_u2", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2, HistoryVisible: 1}})
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if activationCalls != 2 || projectionCalls != 1 || listCalls != 2 {
		t.Fatalf("activation/projection/list calls = %d/%d/%d, want 2/1/2", activationCalls, projectionCalls, listCalls)
	}
	if len(got) != 1 || got[0].ChannelID != "g1" || got[0].LastMsgSeq != 3 {
		t.Fatalf("conversations = %+v, want rebuilt g1", got)
	}
}

func TestIMSyncUserConversationV3DoesNotReactivateExistingBusinessGroup(t *testing.T) {
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g1","channel_type":2,"active_at":1000000,"unread":0}],"done":true}`))
		case "/conversations/activate":
			t.Error("existing group was activated again")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})
	got, err := ctx.IMSyncUserConversation("sync_u3", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChannelID != "g1" {
		t.Fatalf("conversations = %+v", got)
	}
}

// 对账必须 fail-open：zeroim 2026-08-24 线上事故——fail-closed 版本里单个修不好的群会让该用户的
// conversation/sync 恒定报错，客户端 3 秒一 retry，iOS 建连流程永远完不成（「无法长连接」），
// 当天两次上线两次紧急回滚。修不好的群只允许缺它自己，其余会话必须照常返回。
func TestIMSyncUserConversationV3FailsOpenWhenBusinessGroupActivationFails(t *testing.T) {
	listCalls := 0
	var mu sync.Mutex
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			mu.Lock()
			listCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g0","channel_type":2,"active_at":0,"unread":0,"last_message":{"message_id":1,"message_idstr":"1","message_seq":5,"from_uid":"u2","client_msg_no":"real-1","server_timestamp_ms":1700000000000,"payload":"eyJ0eXBlIjoxfQ=="}}],"done":true}`))
		case "/conversations/activate":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"msg":"retry required"}`))
		default:
			http.NotFound(w, r)
		}
	})
	got, err := ctx.IMSyncUserConversation("sync_u4", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
	if err != nil {
		t.Fatalf("activation failure must degrade, not fail the whole sync: %v", err)
	}
	if len(got) != 1 || got[0].ChannelID != "g0" {
		t.Fatalf("existing conversations must survive a failed repair, got %+v", got)
	}
	if listCalls != 1 {
		t.Fatalf("list calls = %d, want no authoritative reread after failed activation", listCalls)
	}
}

func TestIMSyncUserConversationV3FailsOpenWhenRepairedGroupIsStillAbsent(t *testing.T) {
	listCalls := 0
	var mu sync.Mutex
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			mu.Lock()
			listCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"conversations":[],"done":true}`))
		case "/conversations/activate":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	})
	got, err := ctx.IMSyncUserConversation("sync_u5", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
	if err != nil {
		t.Fatalf("still-missing repaired group must degrade, not fail the whole sync: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("conversations = %+v, want empty degraded result", got)
	}
	if listCalls != 2 {
		t.Fatalf("list calls = %d, want authoritative post-repair reread", listCalls)
	}
}

// 重建顶格占位（hole-）在频道末尾时，会话预览必须回退成往前第一条真实消息，
// 且水位（LastMsgSeq/LastClientMsgNo）也要落在真实消息上。
func TestIMSyncUserConversationV3ResolvesPlaceholderTail(t *testing.T) {
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g9","channel_type":2,"active_at":0,"unread":0,"last_message":{"message_id":99,"message_idstr":"99","message_seq":12,"from_uid":"u_10000","client_msg_no":"hole-99","server_timestamp_ms":1700000012000,"payload":"e30="}}],"done":true}`))
		case "/channel/messagesync":
			_, _ = w.Write([]byte(`{"start_message_seq":12,"end_message_seq":0,"messages":[` +
				`{"header":{},"message_id":11,"message_idstr":"11","message_seq":11,"client_msg_no":"hole-98","from_uid":"u_10000","channel_id":"g9","channel_type":2,"timestamp":1700000011,"payload":"e30="},` +
				`{"header":{},"message_id":10,"message_idstr":"10","message_seq":10,"client_msg_no":"real-7","from_uid":"u2","channel_id":"g9","channel_type":2,"timestamp":1700000010,"payload":"eyJ0eXBlIjoxfQ=="}` +
				`]}`))
		default:
			http.NotFound(w, r)
		}
	})
	got, err := ctx.IMSyncUserConversation("sync_u6", 0, 1, "", nil)
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("conversations = %+v, want one", got)
	}
	conv := got[0]
	if conv.LastMsgSeq != 10 || conv.LastClientMsgNo != "real-7" {
		t.Fatalf("conversation watermark = seq %d / %q, want 10 / real-7", conv.LastMsgSeq, conv.LastClientMsgNo)
	}
	if len(conv.Recents) != 1 || conv.Recents[0].ClientMsgNo != "real-7" {
		t.Fatalf("recents = %+v, want the resolved real message", conv.Recents)
	}
}

// 断线漏收契约：unread>0 的会话即使不在最近活跃 30 个里，也必须随会话同步补拉最近消息
// （v2 语义；安卓不会主动补中间的洞，漏了就是「发五条只见一两条」）。
func TestIMSyncUserConversationV3BackfillsUnreadConversationsBeyondRecentWindow(t *testing.T) {
	var mu sync.Mutex
	synced := map[string]bool{}
	ctx := newConvSyncV3Context(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			var sb []byte
			sb = append(sb, []byte(`{"conversations":[`)...)
			// 40 个会话：g0 最老且 unread=3，其余 unread=0
			for i := 0; i < 40; i++ {
				if i > 0 {
					sb = append(sb, ',')
				}
				unread := 0
				if i == 0 {
					unread = 3
				}
				sb = append(sb, []byte(fmt.Sprintf(
					`{"channel_id":"g%d","channel_type":2,"active_at":%d,"unread":%d,"last_message":{"message_id":%d,"message_idstr":"%d","message_seq":9,"from_uid":"u2","client_msg_no":"m%d","server_timestamp_ms":%d,"payload":"e30="}}`,
					i, 1700000000000+int64(i)*1000, unread, i+1, i+1, i, 1700000000000+int64(i)*1000))...)
			}
			sb = append(sb, []byte(`],"done":true}`)...)
			_, _ = w.Write(sb)
		case "/channel/messagesync":
			var req struct {
				ChannelID string `json:"channel_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			synced[req.ChannelID] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"messages":[{"header":{},"message_id":1,"message_idstr":"1","message_seq":9,"client_msg_no":"real-x","from_uid":"u2","channel_id":"` + req.ChannelID + `","channel_type":2,"timestamp":1700000000,"payload":"eyJ0eXBlIjoxfQ=="}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	got, err := ctx.IMSyncUserConversation("sync_u7", 0, 5, "", nil)
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("conversations = %d, want 40", len(got))
	}
	if !synced["g0"] {
		t.Fatal("unread conversation g0 outside the recent-30 window must be backfilled")
	}
	if !synced["g39"] || !synced["g10"] {
		t.Fatal("recent-window conversations must still be backfilled")
	}
	if synced["g1"] {
		t.Fatal("zero-unread conversation outside the window must not be backfilled")
	}
}
