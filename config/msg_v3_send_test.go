package config

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/common"
	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

func TestAdaptSendReqV3FillsSystemUIDWhenFromUIDEmpty(t *testing.T) {
	// 群提示消息（建群/入群/退群/解散）都不带 from_uid，v3 会 400，
	// 结果是新建的群一条消息都没有，客户端会话列表里就看不到这个群。
	got := adaptSendReqV3(&MsgSendReq{
		Header:      MsgHeader{NoPersist: 0, RedDot: 1},
		ChannelID:   "g_1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte("tip"),
	}, "u_10000")

	if got.FromUID != "u_10000" {
		t.Fatalf("from_uid = %q, want 系统账号 u_10000", got.FromUID)
	}
	if got.ChannelID != "g_1" || got.ChannelType != common.ChannelTypeGroup.Uint8() {
		t.Fatalf("群频道消息不该被改写: %+v", got)
	}
	if len(got.Subscribers) != 0 {
		t.Fatalf("subscribers = %#v, want 空", got.Subscribers)
	}
}

func TestAdaptSendReqV3RewritesPersonCMDToSubscribers(t *testing.T) {
	// 单聊 CMD（清红点、已读回执、正在输入）在 v3 走频道会 400/503，改走定向投递。
	got := adaptSendReqV3(&MsgSendReq{
		Header:      MsgHeader{NoPersist: 1, SyncOnce: 1},
		FromUID:     "u_a",
		ChannelID:   "u_b",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte("cmd"),
	}, "u_10000")

	if !reflect.DeepEqual(got.Subscribers, []string{"u_b"}) {
		t.Fatalf("subscribers = %#v, want [u_b]", got.Subscribers)
	}
	if got.ChannelID != "" || got.ChannelType != 0 {
		t.Fatalf("v3 要求 subscribers 与 channel_id 互斥，实际 %+v", got)
	}
	if got.Header.SyncOnce != 1 {
		t.Fatalf("sync_once = %d, want 1", got.Header.SyncOnce)
	}
	if got.FromUID != "u_a" {
		t.Fatalf("from_uid = %q, want 保留调用方的 u_a", got.FromUID)
	}
}

func TestAdaptSendReqV3ClearsChannelWhenSubscribersGiven(t *testing.T) {
	// 群已读回执本来就带 subscribers，v3 里 subscribers 不能和 channel_id 同时出现。
	got := adaptSendReqV3(&MsgSendReq{
		Header:      MsgHeader{NoPersist: 1, SyncOnce: 1},
		ChannelID:   "g_1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Subscribers: []string{"u_a", "", "u_b", "u_a"},
		Payload:     []byte("cmd"),
	}, "u_10000")

	if got.ChannelID != "" || got.ChannelType != 0 {
		t.Fatalf("channel_id/channel_type 应被清空，实际 %+v", got)
	}
	if !reflect.DeepEqual(got.Subscribers, []string{"u_a", "u_b"}) {
		t.Fatalf("subscribers = %#v, want 去重去空后的 [u_a u_b]", got.Subscribers)
	}
	if got.FromUID != "u_10000" {
		t.Fatalf("from_uid = %q, want u_10000", got.FromUID)
	}
}

func TestAdaptSendReqV3LeavesPersistentPersonMessageAlone(t *testing.T) {
	// 普通单聊消息是持久化的，走频道那条路在 v3 是好的，不能被改写成定向投递，
	// 否则消息不落频道时间线，历史里就没有这条。
	got := adaptSendReqV3(&MsgSendReq{
		Header:      MsgHeader{NoPersist: 0, RedDot: 1},
		FromUID:     "u_a",
		ChannelID:   "u_b",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte("hi"),
	}, "u_10000")

	if got.ChannelID != "u_b" || got.ChannelType != common.ChannelTypePerson.Uint8() {
		t.Fatalf("持久化单聊消息不该被改写: %+v", got)
	}
	if len(got.Subscribers) != 0 {
		t.Fatalf("subscribers = %#v, want 空", got.Subscribers)
	}
}

func TestAdaptSendReqV3DoesNotMutateCaller(t *testing.T) {
	req := &MsgSendReq{
		Header:      MsgHeader{NoPersist: 1, SyncOnce: 1},
		ChannelID:   "u_b",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte("cmd"),
	}
	adaptSendReqV3(req, "u_10000")

	if req.ChannelID != "u_b" || req.FromUID != "" || len(req.Subscribers) != 0 {
		t.Fatalf("调用方的请求被改坏了: %+v", req)
	}
}

func TestSendMessageWithResultV3RejectsHTTP200WithFailureReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":0,"message_seq":0,"reason":13}`))
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	if _, err := ctx.SendMessageWithResult(&MsgSendReq{
		ChannelID:   "g_1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     []byte("tip"),
	}); err == nil {
		t.Fatal("reason=13 被当成成功，want error")
	}
}

func TestSendGroupMemberAddV3ActivatesConversationBeforeSendingTip(t *testing.T) {
	type activation struct {
		UID         string `json:"uid"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/conversations/activate":
			var got activation
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("解析 activate 请求失败: %v", err)
			}
			if got.UID != "u_new" || got.ChannelID != "g_1" || got.ChannelType != common.ChannelTypeGroup.Uint8() {
				t.Fatalf("activate 请求 = %+v", got)
			}
			calls = append(calls, "activate")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/message/send":
			var got capturedSend
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("解析 send 请求失败: %v", err)
			}
			if got.FromUID != "u_10000" {
				t.Fatalf("入群提示 from_uid = %q, want u_10000", got.FromUID)
			}
			calls = append(calls, "send")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"message_id":1,"message_seq":1,"reason":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	err := ctx.SendGroupMemberAdd(&MsgGroupMemberAddReq{
		GroupNo:      "g_1",
		Operator:     "u_owner",
		OperatorName: "owner",
		Members:      []*UserBaseVo{{UID: "u_new", Name: "new"}},
	})
	if err != nil {
		t.Fatalf("SendGroupMemberAdd() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"activate", "send"}) {
		t.Fatalf("调用顺序 = %v, want [activate send]", calls)
	}
}

func TestSendGroupMemberAddV3MutedTipStillActivatesConversation(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/conversations/activate":
			calls = append(calls, "activate")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/message/send":
			calls = append(calls, "send")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"message_id":1,"message_seq":1,"reason":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	ctx.SetMemberAddTipMuter(func(groupNo string) bool { return groupNo == "g_1" })

	err := ctx.SendGroupMemberAdd(&MsgGroupMemberAddReq{
		GroupNo:      "g_1",
		Operator:     "u_owner",
		OperatorName: "owner",
		Members:      []*UserBaseVo{{UID: "u_new", Name: "new"}},
	})
	if err != nil {
		t.Fatalf("SendGroupMemberAdd() error = %v", err)
	}
	// 静默的只是提示消息；会话激活必须照常发生，否则新成员看不到这个群
	if !reflect.DeepEqual(calls, []string{"activate"}) {
		t.Fatalf("调用 = %v, want [activate]（提示应被静默）", calls)
	}
}

func TestSendGroupMemberAddV3ReturnsActivationFailureForOutboxRetry(t *testing.T) {
	sentTip := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations/activate":
			http.Error(w, `{"msg":"retry required"}`, http.StatusServiceUnavailable)
		case "/message/send":
			sentTip = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"message_id":1,"message_seq":1,"reason":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	err := ctx.SendGroupMemberAdd(&MsgGroupMemberAddReq{
		GroupNo: "g_1", Members: []*UserBaseVo{{UID: "u_new"}},
	})
	if err == nil {
		t.Fatal("activation failure was reported as success")
	}
	if sentTip {
		t.Fatal("tip was sent before the conversation activation contract succeeded")
	}
}

func TestWithSystemUIDInWhitelist(t *testing.T) {
	if got := withSystemUIDInWhitelist(nil, "u_10000"); got != nil {
		t.Fatalf("空白名单被改成 %+v，会意外开启全员禁言", got)
	}
	if got := withSystemUIDInWhitelist([]string{"u_owner"}, "u_10000"); !reflect.DeepEqual(got, []string{"u_owner", "u_10000"}) {
		t.Fatalf("白名单 = %+v, want [u_owner u_10000]", got)
	}
	if got := withSystemUIDInWhitelist([]string{"u_owner", "u_10000"}, "u_10000"); !reflect.DeepEqual(got, []string{"u_owner", "u_10000"}) {
		t.Fatalf("系统号被重复加入: %+v", got)
	}
}

func TestChunkSubscribers(t *testing.T) {
	uids := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		uids = append(uids, string(rune('a'+i)))
	}
	for _, tc := range []struct {
		name string
		in   []string
		size int
		want [][]string
	}{
		{name: "空列表", in: nil, size: 3, want: nil},
		{name: "不足一批", in: uids[:2], size: 3, want: [][]string{{"a", "b"}}},
		{name: "整除", in: uids[:6], size: 3, want: [][]string{{"a", "b", "c"}, {"d", "e", "f"}}},
		{name: "有余数", in: uids, size: 3, want: [][]string{{"a", "b", "c"}, {"d", "e", "f"}, {"g"}}},
		{name: "size非正数不切", in: uids[:2], size: 0, want: [][]string{{"a", "b"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkSubscribers(tc.in, tc.size); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("chunkSubscribers(%v, %d) = %v, want %v", tc.in, tc.size, got, tc.want)
			}
		})
	}
}

func TestAdaptSendReqV3SplitsFakeChannelIDForPersonCMD(t *testing.T) {
	// 调用方误传库内 fake id（a@b）时，直接当 uid 定向投递等于发给一个不存在的人，
	// 静默无投递也无报错；拆成两端投递才等价于 v2 的单聊频道语义。
	got := adaptSendReqV3(&MsgSendReq{
		Header:      MsgHeader{NoPersist: 1, SyncOnce: 1},
		FromUID:     "u_a",
		ChannelID:   "u_a@u_b",
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     []byte("cmd"),
	}, "u_10000")

	if !reflect.DeepEqual(got.Subscribers, []string{"u_a", "u_b"}) {
		t.Fatalf("subscribers = %#v, want [u_a u_b]", got.Subscribers)
	}
	if got.ChannelID != "" || got.ChannelType != 0 {
		t.Fatalf("v3 要求 subscribers 与 channel_id 互斥，实际 %+v", got)
	}
}

func TestIMSyncChannelMessageV3BackfillsPullModeFromRequest(t *testing.T) {
	// v3 的 /channel/messagesync 响应体不带 pull_mode，反序列化恒为 0（PullModeDown），
	// 客户端上拉历史时拿到的 pull_mode 就是错的，必须按请求回填。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"start_message_seq":10,"end_message_seq":1,"more":1,"messages":[]}`))
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	resp, err := ctx.IMSyncChannelMessage(SyncChannelMessageReq{
		LoginUID:        "u_a",
		ChannelID:       "u_b",
		ChannelType:     common.ChannelTypePerson.Uint8(),
		StartMessageSeq: 10,
		EndMessageSeq:   1,
		PullMode:        PullModeUp,
	})
	if err != nil {
		t.Fatalf("IMSyncChannelMessage: %v", err)
	}
	if resp.PullMode != PullModeUp {
		t.Fatalf("pull_mode = %d, want PullModeUp(%d)", resp.PullMode, PullModeUp)
	}
}

func TestIMSOnlineStatusEmptyUIDsShortCircuits(t *testing.T) {
	// v3 对空 uids 返回对象 {"status":200} 而不是数组，反序列化必炸；
	// 空入参应该直接短路返回空结果，不发请求。
	cfg := New()
	cfg.WuKongIM.APIURL = "http://127.0.0.1:1" // 不可达，一旦真发请求就会失败
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	resps, err := ctx.IMSOnlineStatus(nil)
	if err != nil {
		t.Fatalf("空入参不该报错: %v", err)
	}
	if len(resps) != 0 {
		t.Fatalf("resps = %#v, want 空", resps)
	}
}

// 建群与拉人同一条红线：v3 会话目录是显式 membership，「创建群聊」提示一旦被
// 拒收（如系统号不受信 reason=3），全体成员在有人发言前都看不到新群。
// 建群路径必须先激活全体成员（含群主）的会话，再发提示。
func TestSendGroupCreateV3ActivatesAllMembersBeforeSendingTip(t *testing.T) {
	type activation struct {
		UID         string `json:"uid"`
		ChannelID   string `json:"channel_id"`
		ChannelType uint8  `json:"channel_type"`
	}
	var activated []string
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/conversations/activate":
			var got activation
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("解析 activate 请求失败: %v", err)
			}
			if got.ChannelID != "g_new" || got.ChannelType != common.ChannelTypeGroup.Uint8() {
				t.Fatalf("activate 请求 = %+v", got)
			}
			activated = append(activated, got.UID)
			calls = append(calls, "activate")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "/message/send":
			calls = append(calls, "send")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"message_id":1,"message_seq":1,"reason":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	err := ctx.SendGroupCreate(&MsgGroupCreateReq{
		GroupNo:     "g_new",
		Creator:     "u_owner",
		CreatorName: "owner",
		Members: []*UserBaseVo{
			{UID: "u_owner", Name: "owner"},
			{UID: "u_a", Name: "a"},
			{UID: "u_b", Name: "b"},
		},
	})
	if err != nil {
		t.Fatalf("SendGroupCreate() error = %v", err)
	}
	if calls[len(calls)-1] != "send" {
		t.Fatalf("最后一步应是发送提示, calls = %v", calls)
	}
	sort.Strings(activated)
	// 群主也必须激活：建群者的会话同样不能依赖 tip 消息
	if !reflect.DeepEqual(activated, []string{"u_a", "u_b", "u_owner"}) {
		t.Fatalf("activate 覆盖成员 = %v, want 全体(含群主)", activated)
	}
}

// activate 失败必须把错误抛回事件 outbox 重试，不得吞掉后继续发提示
func TestSendGroupCreateV3ReturnsActivationFailureForOutboxRetry(t *testing.T) {
	sentTip := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations/activate":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"msg":"boom"}`))
		case "/message/send":
			sentTip = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"message_id":1,"message_seq":1,"reason":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}

	err := ctx.SendGroupCreate(&MsgGroupCreateReq{
		GroupNo:     "g_new",
		Creator:     "u_owner",
		CreatorName: "owner",
		Members:     []*UserBaseVo{{UID: "u_owner", Name: "owner"}, {UID: "u_a", Name: "a"}},
	})
	if err == nil {
		t.Fatal("activate 失败应返回错误交给事件重试")
	}
	if sentTip {
		t.Fatal("activate 失败后不应继续发送建群提示")
	}
}
