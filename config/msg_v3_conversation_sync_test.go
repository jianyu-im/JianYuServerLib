package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

func TestIMSyncUserConversationV3FreshInstallReconcilesBusinessGroups(t *testing.T) {
	var mu sync.Mutex
	listCalls := 0
	activationCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			mu.Lock()
			listCalls++
			call := listCalls
			mu.Unlock()
			if call == 1 {
				_, _ = w.Write([]byte(`{"conversations":[],"done":true,"coverage":9}`))
				return
			}
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g1","channel_type":2,"active_at":1000000,"unread":1,"last_message":{"message_id":8,"message_idstr":"8","message_seq":8,"from_uid":"u2","client_msg_no":"m8","server_timestamp_ms":1008000,"payload":"e30="}}],"done":true,"coverage":10}`))
		case "/channel/messagesync":
			// 对账探测：membership 在，返回一条消息
			_, _ = w.Write([]byte(`{"messages":[{"header":{},"message_id":8,"message_idstr":"8","message_seq":8,"client_msg_no":"m8","from_uid":"u2","channel_id":"g1","channel_type":2,"timestamp":1008,"payload":"e30="}]}`))
		case "/conversations/activate":
			var req struct {
				UID         string `json:"uid"`
				ChannelID   string `json:"channel_id"`
				ChannelType uint8  `json:"channel_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode activation: %v", err)
			}
			if req.UID != "u1" || req.ChannelID != "g1" || req.ChannelType != 2 {
				t.Errorf("activation = %+v", req)
			}
			mu.Lock()
			activationCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		case "/channel/messagesync":
			// 对账探测：频道在、本人缺行（activate 是异步提案恒 200，不能作判据）
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"msg":"internal/message: valid channel membership required"}`))
		case "/conversations/activate":
			mu.Lock()
			activationCalls++
			mu.Unlock()
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
			if req.ChannelID != "g1" || req.ChannelType != 2 || req.HistoryVisible != 1 || len(req.Subscribers) != 1 || req.Subscribers[0] != "u1" {
				t.Errorf("membership projection = %+v", req)
			}
			mu.Lock()
			projectionCalls++
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2, HistoryVisible: 1}})
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if activationCalls != 1 || projectionCalls != 1 || listCalls != 2 {
		t.Fatalf("activation/projection/list calls = %d/%d/%d, want 1/1/2", activationCalls, projectionCalls, listCalls)
	}
	if len(got) != 1 || got[0].ChannelID != "g1" || got[0].LastMsgSeq != 3 {
		t.Fatalf("conversations = %+v, want rebuilt g1", got)
	}
}

func TestIMSyncUserConversationV3DoesNotReactivateExistingBusinessGroup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChannelID != "g1" {
		t.Fatalf("conversations = %+v", got)
	}
}

// 对账必须 fail-open：2026-08-24 线上事故——fail-closed 版本里单个修不好的群会让该用户的
// conversation/sync 恒定报错，客户端 3 秒一 retry，iOS 建连流程永远完不成（「无法长连接」），
// 当天两次上线两次紧急回滚。修不好的群只允许缺它自己，其余会话必须照常返回。
func TestIMSyncUserConversationV3FailsOpenWhenBusinessGroupActivationFails(t *testing.T) {
	listCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			listCalls++
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g0","channel_type":2,"active_at":0,"unread":0,"last_message":{"message_id":1,"message_idstr":"1","message_seq":5,"from_uid":"u2","client_msg_no":"real-1","server_timestamp_ms":1700000000000,"payload":"eyJ0eXBlIjoxfQ=="}}],"done":true}`))
		case "/channel/messagesync":
			_, _ = w.Write([]byte(`{"messages":[{"header":{},"message_id":1,"message_idstr":"1","message_seq":5,"client_msg_no":"real-1","from_uid":"u2","channel_id":"g1","channel_type":2,"timestamp":1700000000,"payload":"e30="}]}`))
		case "/conversations/activate":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"msg":"retry required"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			listCalls++
			_, _ = w.Write([]byte(`{"conversations":[],"done":true}`))
		case "/channel/messagesync":
			_, _ = w.Write([]byte(`{"messages":[{"header":{},"message_id":1,"message_idstr":"1","message_seq":3,"client_msg_no":"real-1","from_uid":"u2","channel_id":"g1","channel_type":2,"timestamp":1700000000,"payload":"e30="}]}`))
		case "/conversations/activate":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "", []*Channel{{ChannelID: "g1", ChannelType: 2}})
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

func TestIMSyncUserConversationV3BackfillsClientKnownPersonMissingFromDirectory(t *testing.T) {
	t.Helper()
	var batchRequest struct {
		LoginUID string `json:"login_uid"`
		Items    []struct {
			ChannelID   string `json:"channel_id"`
			ChannelType uint8  `json:"channel_type"`
			Limit       int    `json:"limit"`
		} `json:"items"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			_, _ = w.Write([]byte(`{"conversations":[],"deletes":[],"unresolved":[],"done":true,"coverage":9,"reset_required":false}`))
		case "/channel/messagesyncbatch":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &batchRequest); err != nil {
				t.Fatalf("decode batch request: %v", err)
			}
			_, _ = w.Write([]byte(`{"items":[{"channel_id":"u2","channel_type":1,"messages":[` +
				`{"header":{"red_dot":1},"message_id":3,"message_idstr":"3","message_seq":3,"client_msg_no":"m3","from_uid":"u2","channel_id":"u2","channel_type":1,"timestamp":1003,"payload":"e30="},` +
				`{"header":{"red_dot":1},"message_id":5,"message_idstr":"5","message_seq":5,"client_msg_no":"m5","from_uid":"u2","channel_id":"u2","channel_type":1,"timestamp":1005,"payload":"e30="},` +
				`{"header":{"red_dot":1},"message_id":6,"message_idstr":"6","message_seq":6,"client_msg_no":"m6","from_uid":"u2","channel_id":"u2","channel_type":1,"timestamp":1006,"payload":"e30="}` +
				`]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "u2:1:4", nil)
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if batchRequest.LoginUID != "u1" || len(batchRequest.Items) != 1 {
		t.Fatalf("batch request = %+v, want one u1 item", batchRequest)
	}
	if item := batchRequest.Items[0]; item.ChannelID != "u2" || item.ChannelType != 1 || item.Limit < 50 {
		t.Fatalf("batch item = %+v, want u2 person fallback with at least 50 messages", item)
	}
	if len(got) != 1 {
		t.Fatalf("conversations = %+v, want one backfilled person conversation", got)
	}
	conv := got[0]
	if conv.ChannelID != "u2" || conv.ChannelType != 1 || conv.LastMsgSeq != 6 || conv.LastClientMsgNo != "m6" {
		t.Fatalf("conversation = %+v, want u2 at seq 6", conv)
	}
	if conv.Unread != 2 {
		t.Fatalf("unread = %d, want two peer messages after local seq 4", conv.Unread)
	}
	// v2 契约：recents 最新在前（recents[0] 是会话最新一条），并按客户端 msg_count(=1) 截断。
	// 2026-08-25 现场：升序透传让各端把窗口最老一条当成会话末条（列表预览显示 18 天前的老消息、
	// winds 整列表乱序），winds 还会对 >20 条的 recents 整批拒收。
	if len(conv.Recents) != 1 || conv.Recents[0].MessageSeq != 6 {
		t.Fatalf("recents = %+v, want newest-first truncated to msg_count=1 (seq 6)", conv.Recents)
	}
}

func TestIMSyncUserConversationV3PersonBackfillFailsOpenOnChannelNotFound(t *testing.T) {
	// 2026-08-25 线上事故回归：客户端 last_msg_seqs 里带着 v3 引擎不认识的 fake channel（a@b），
	// messagesyncbatch 对该条返回 channel not found，此时只该缺这一个会话，
	// 其余会话（目录里已有的 + 能补出来的单聊）必须原样返回，整个 sync 不能失败。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"u9","channel_type":1,"active_at":1000000,"unread":0,"last_message":{"message_id":2,"message_idstr":"2","message_seq":2,"from_uid":"u9","client_msg_no":"m2","server_timestamp_ms":1002000,"payload":"e30="}}],"done":true,"coverage":10}`))
		case "/channel/messagesyncbatch":
			_, _ = w.Write([]byte(`{"items":[` +
				`{"channel_id":"a@b","channel_type":1,"error":"channel not found"},` +
				`{"channel_id":"u2","channel_type":1,"messages":[{"header":{"red_dot":1},"message_id":6,"message_idstr":"6","message_seq":6,"client_msg_no":"m6","from_uid":"u2","channel_id":"u2","channel_type":1,"timestamp":1006,"payload":"e30="}]}` +
				`]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "a@b:1:4|u2:1:4", nil)
	if err != nil {
		t.Fatalf("channel-not-found backfill must fail open, not fail the whole sync: %v", err)
	}
	byChannel := make(map[string]*SyncUserConversationResp, len(got))
	for _, conv := range got {
		byChannel[conv.ChannelID] = conv
	}
	if _, ok := byChannel["a@b"]; ok {
		t.Fatalf("conversations = %+v, fake channel a@b must be dropped, not fabricated", got)
	}
	if _, ok := byChannel["u9"]; !ok {
		t.Fatalf("conversations = %+v, directory conversation u9 must survive", got)
	}
	if conv, ok := byChannel["u2"]; !ok || conv.LastMsgSeq != 6 {
		t.Fatalf("conversations = %+v, healthy backfill u2 must still land at seq 6", got)
	}
}

func TestParseConversationSyncAnchorsV3PreservesThirdPartyUIDWithColons(t *testing.T) {
	got := parseConversationSyncAnchorsV3("sso:tenant:user@example.com:1:42|group-1:2:9")
	if len(got) != 2 {
		t.Fatalf("anchors = %+v, want two", got)
	}
	if got[0].ChannelID != "sso:tenant:user@example.com" || got[0].ChannelType != 1 || got[0].LastSeq != 42 {
		t.Fatalf("third-party anchor = %+v", got[0])
	}
	if got[1].ChannelID != "group-1" || got[1].ChannelType != 2 || got[1].LastSeq != 9 {
		t.Fatalf("group anchor = %+v", got[1])
	}
}

func TestIMSyncUserConversationV3FailsClosedWhenDirectoryIsUnresolved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list", "/conversation/retry":
			_, _ = w.Write([]byte(`{"conversations":[],"deletes":[],"unresolved":[{"channel_id":"u2","channel_type":1}],"done":true,"coverage":9,"reset_required":false}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	if _, err := ctx.IMSyncUserConversation("u1", 0, 1, "", nil); err == nil {
		t.Fatal("unresolved directory was reported as a successful empty sync")
	}
}

func TestIMSyncChannelMessageV3DoesNotHidePersonMembershipProjectionLag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"msg":"internal/message: valid channel membership required","status":400}`))
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	if _, err := ctx.IMSyncChannelMessage(SyncChannelMessageReq{
		LoginUID: "u1", ChannelID: "u2", ChannelType: 1, Limit: 20,
	}); err == nil {
		t.Fatal("person membership projection lag was hidden as an empty timeline")
	}
}

// winds PC 内核对会话同步有两级 version 闸门（批级 batch.version <= 本地水位整批丢弃、
// 行级 incoming.version < existing.version 保留旧行），且本地水位是 v2 时代的纳秒版本号。
// v3 适配层若下发 Version=0，winds 切 v3 后每次会话同步都被静默判旧——会话列表冻结、
// 与聊天窗口对不上。回归口径：Version 必须是纳秒量纲且随最后一条消息时间单调。
func TestIMSyncUserConversationV3VersionExceedsV2NanoWatermark(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			// server_timestamp_ms 用现网真实量级（2026-08-24 14:29:20.707）
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"u2","channel_type":1,"active_at":0,"unread":1,"last_message":{"message_id":84,"message_idstr":"84","message_seq":84,"from_uid":"u2","client_msg_no":"m84","server_timestamp_ms":1787552960707,"payload":"e30="}}],"done":true,"coverage":9}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncUserConversation("u1", 0, 1, "", nil)
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("conversations = %+v, want one", got)
	}
	conv := got[0]
	wantVersion := int64(1787552960707) * 1e6 // 毫秒 ×1e6 = 纳秒量纲
	if conv.Version != wantVersion {
		t.Fatalf("version = %d, want %d (ns-scale of last message)", conv.Version, wantVersion)
	}
	// 必须严格大于 winds 现场实测的 v2 纳秒水位（秒×1e9），否则整批被 StaleSync 丢弃
	const v2Watermark = int64(1787400013000000000)
	if conv.Version <= v2Watermark {
		t.Fatalf("version = %d, want > v2 nano watermark %d", conv.Version, v2Watermark)
	}
	if conv.Timestamp != 1787552960 {
		t.Fatalf("timestamp = %d, want seconds of last message", conv.Timestamp)
	}
}

func TestConversationVersionV3Scales(t *testing.T) {
	cases := []struct {
		name                  string
		activeAt, lastMessage int64
		want                  int64
	}{
		{"皆空为0", 0, 0, 0},
		{"毫秒消息时间", 0, 1787552960707, 1787552960707 * int64(1e6)},
		{"秒量纲归一化", 0, 1787552960, 1787552960000 * int64(1e6)},
		{"纳秒active_at兜底", 1787552960707000000, 0, 1787552960707 * int64(1e6)},
		{"取较大者", 1787552960707000000, 1787552999000, 1787552999000 * int64(1e6)},
	}
	for _, c := range cases {
		if got := conversationVersionV3(c.activeAt, c.lastMessage); got != c.want {
			t.Fatalf("%s: conversationVersionV3(%d,%d) = %d, want %d", c.name, c.activeAt, c.lastMessage, got, c.want)
		}
	}
}

// 断线漏收契约：unread>0 的会话必须随会话同步补拉最近 msgCount 条消息
// （v2 语义；安卓不会主动补中间的洞，漏了就是「发五条只见一两条」）。
func TestIMSyncUserConversationV3BackfillsUnreadConversations(t *testing.T) {
	var mu sync.Mutex
	synced := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_, _ = w.Write([]byte(`{"messages":[` +
				`{"header":{},"message_id":7,"message_idstr":"7","message_seq":7,"client_msg_no":"real-7","from_uid":"u2","channel_id":"` + req.ChannelID + `","channel_type":2,"timestamp":1700000000,"payload":"eyJ0eXBlIjoxfQ=="},` +
				`{"header":{},"message_id":9,"message_idstr":"9","message_seq":9,"client_msg_no":"real-9","from_uid":"u2","channel_id":"` + req.ChannelID + `","channel_type":2,"timestamp":1700000002,"payload":"eyJ0eXBlIjoxfQ=="}` +
				`]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncUserConversation("sync_u7", 0, 5, "", nil)
	if err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("conversations = %d, want 40", len(got))
	}
	if !synced["g0"] {
		t.Fatal("unread conversation g0 must be backfilled with recent messages")
	}
	if synced["g39"] || synced["g1"] {
		t.Fatal("zero-unread conversations must not be backfilled")
	}
	for _, conv := range got {
		if conv.ChannelID != "g0" {
			continue
		}
		// v2 契约：recents 最新在前，recents[0] 必须是会话最新一条（各端与 winds 内核都按此消费）。
		if len(conv.Recents) != 2 || conv.Recents[0].MessageSeq != 9 || conv.Recents[1].MessageSeq != 7 {
			t.Fatalf("g0 recents = %+v, want backfilled newest-first seq 9,7", conv.Recents)
		}
	}
}

// 频道整体缺失的重建契约：探测报 channel not found → 按业务成员投影全员 →
// 补种子消息立日志（无日志频道会被会话水合当已删除丢弃）→ 激活。
func TestIMSyncUserConversationV3RebuildsMissingChannelWithSeedMessage(t *testing.T) {
	var mu sync.Mutex
	var projectedUIDs []string
	seedSends := 0
	activations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			_, _ = w.Write([]byte(`{"conversations":[],"done":true}`))
		case "/channel/messagesync":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"msg":"internal/message: channel not found: channel: channel not found"}`))
		case "/channel/subscriber_add":
			var req struct {
				Subscribers    []string `json:"subscribers"`
				HistoryVisible int      `json:"history_visible"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			projectedUIDs = append(projectedUIDs, req.Subscribers...)
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		case "/message/send":
			mu.Lock()
			seedSends++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"message_id":1,"message_seq":1,"reason":1}`))
		case "/conversations/activate":
			mu.Lock()
			activations++
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	ctx.SetGroupMemberProvider(func(groupNo string) ([]string, error) {
		return []string{"sync_u8", "member_b", "member_c"}, nil
	})
	if _, err := ctx.IMSyncUserConversation("sync_u8", 0, 1, "", []*Channel{{ChannelID: "g_missing", ChannelType: 2, HistoryVisible: 1}}); err != nil {
		t.Fatalf("IMSyncUserConversation() error = %v", err)
	}
	if len(projectedUIDs) != 3 {
		t.Fatalf("projected uids = %v, want all 3 business members", projectedUIDs)
	}
	if seedSends != 1 {
		t.Fatalf("seed sends = %d, want exactly one invisible seed message", seedSends)
	}
	if activations != 1 {
		t.Fatalf("activations = %d, want 1", activations)
	}
}

// 单聊 membership 缺行自愈契约：messagesync 撞 membership required 时调
// /conversations/membership/repair 补行，300ms 后重试一次原请求成功。
func TestIMSyncChannelMessageV3SelfHealsPersonMembership(t *testing.T) {
	var mu sync.Mutex
	repaired := false
	repairCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/channel/messagesync":
			mu.Lock()
			ok := repaired
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"msg":"internal/message: valid channel membership required"}`))
				return
			}
			_, _ = w.Write([]byte(`{"messages":[{"header":{},"message_id":3,"message_idstr":"3","message_seq":3,"client_msg_no":"m3","from_uid":"heal_u2","channel_id":"heal_u2","channel_type":1,"timestamp":1003,"payload":"e30="}]}`))
		case "/conversations/membership/repair":
			var req struct {
				UID         string `json:"uid"`
				ChannelID   string `json:"channel_id"`
				ChannelType uint8  `json:"channel_type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.UID != "heal_u1" || req.ChannelID != "heal_u2" || req.ChannelType != 1 {
				t.Errorf("repair request = %+v", req)
			}
			mu.Lock()
			repairCalls++
			repaired = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	got, err := ctx.IMSyncChannelMessage(SyncChannelMessageReq{
		LoginUID: "heal_u1", ChannelID: "heal_u2", ChannelType: 1, Limit: 20,
	})
	if err != nil {
		t.Fatalf("IMSyncChannelMessage() error = %v, want self-healed success", err)
	}
	if repairCalls != 1 {
		t.Fatalf("repair calls = %d, want 1", repairCalls)
	}
	if got == nil || len(got.Messages) != 1 || got.Messages[0].MessageSeq != 3 {
		t.Fatalf("messages = %+v, want the real timeline after repair", got)
	}
}
