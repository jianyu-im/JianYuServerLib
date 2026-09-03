package config

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

func newNativeTestContext(t *testing.T, handler http.HandlerFunc) *Context {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	return &Context{cfg: cfg, Log: log.NewTLog("test")}
}

func TestV3NormalizeEpochMSAcrossScales(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"零", 0, 0},
		{"负数当零", -5, 0},
		{"秒", 1787556650, 1787556650000},
		{"毫秒", 1787556650006, 1787556650006},
		{"微秒", 1787556650006265, 1787556650006},
		{"纳秒", 1787556650006265796, 1787556650006},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := V3NormalizeEpochMS(tc.in); got != tc.want {
				t.Fatalf("V3NormalizeEpochMS(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// active_at 只由 /conversations/activate 写，发消息不 bump 它，所以绝大多数会话是 0。
// 排序权威必须靠末条消息时间兜底，否则整张列表全是 0、顺序等于随机。
func TestV3ConversationActiveAtMSFallsBackToLastMessage(t *testing.T) {
	if got := V3ConversationActiveAtMS(0, 1787556650006); got != 1787556650006 {
		t.Fatalf("active_at=0 时应取末条消息时间, got %d", got)
	}
	// activate 写的是纳秒；归一化后仍应小于更新的末条消息时间
	if got := V3ConversationActiveAtMS(1787556650006265796, 1787556999000); got != 1787556999000 {
		t.Fatalf("应取两者较大值, got %d", got)
	}
	if got := V3ConversationActiveAtMS(1787556650006265796, 0); got != 1787556650006 {
		t.Fatalf("骨架会话应回落到归一化后的 active_at, got %d", got)
	}
	if got := V3ConversationActiveAtMS(0, 0); got != 0 {
		t.Fatalf("两者都没有时应为 0（客户端排最后），got %d", got)
	}
}

func TestV3ConversationListPassesThroughCursorAndFlags(t *testing.T) {
	var gotBody map[string]interface{}
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/conversation/list" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"conversations":[{"channel_id":"u2","channel_type":1,"active_at":0,"read_seq":7,"deleted_to_seq":3,"unread":2,
				"last_message":{"message_id":9,"message_idstr":"9","message_seq":12,"from_uid":"u2","client_msg_no":"m9","server_timestamp_ms":1787556650006,"payload":"e30="}}],
			"deletes":[{"channel_id":"g9","channel_type":2}],
			"unresolved":[{"channel_id":"g8","channel_type":2}],
			"next_cursor":"CURSOR2","done":false,"coverage":42,"reset_required":false}`))
	})

	page, err := ctx.V3ConversationList(V3ConversationListReq{UID: "u1", Cursor: "CURSOR1", Limit: 200, CompletedCoverage: 41})
	if err != nil {
		t.Fatalf("V3ConversationList: %v", err)
	}
	if gotBody["cursor"] != "CURSOR1" || gotBody["uid"] != "u1" {
		t.Fatalf("请求体未透传游标/uid: %+v", gotBody)
	}
	if _, ok := gotBody["completed_coverage"]; !ok {
		t.Fatalf("completed_coverage 未透传: %+v", gotBody)
	}
	if page.NextCursor != "CURSOR2" || page.Done {
		t.Fatalf("游标/done 未原样返回: %+v", page)
	}
	if len(page.Conversations) != 1 || len(page.Deletes) != 1 || len(page.Unresolved) != 1 {
		t.Fatalf("三个集合应各一条: %d/%d/%d", len(page.Conversations), len(page.Deletes), len(page.Unresolved))
	}
	conv := page.Conversations[0]
	if conv.ChannelID != "u2" || conv.ChannelType != 1 {
		t.Fatalf("频道标识错: %+v", conv)
	}
	if conv.ReadSeq != 7 || conv.DeletedToSeq != 3 || conv.Unread != 2 {
		t.Fatalf("水位/未读未透传: %+v", conv)
	}
	if conv.ActiveAtMS != 1787556650006 {
		t.Fatalf("active_at_ms 应回落到末条消息时间, got %d", conv.ActiveAtMS)
	}
	if conv.LastMessage.MessageSeqStr != "12" {
		t.Fatalf("seq 字符串形式缺失: %+v", conv.LastMessage)
	}
}

// u64 的 seq 在 JSON number 里超过 2^53 就会静默丢精度，所以响应必须带字符串形式，
// 且字符串形式必须是精确值——这条断言就是「客户端一律读 *_str」契约的服务端一半。
func TestV3ConversationListKeepsU64SeqExact(t *testing.T) {
	const bigSeq uint64 = 9007199254740995 // 2^53 + 3
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"g1","channel_type":2,"read_seq":` +
			strconv.FormatUint(bigSeq, 10) + `,"last_message":{"message_idstr":"9","message_seq":` +
			strconv.FormatUint(bigSeq, 10) + `,"server_timestamp_ms":1787556650006}}],"done":true}`))
	})
	page, err := ctx.V3ConversationList(V3ConversationListReq{UID: "u1"})
	if err != nil {
		t.Fatalf("V3ConversationList: %v", err)
	}
	conv := page.Conversations[0]
	if conv.ReadSeq != bigSeq {
		t.Fatalf("read_seq 精度丢失: got %d want %d", conv.ReadSeq, bigSeq)
	}
	if conv.LastMessage.MessageSeqStr != strconv.FormatUint(bigSeq, 10) {
		t.Fatalf("message_seq_str 不精确: %s", conv.LastMessage.MessageSeqStr)
	}
}

// reset_required 是正常流程信号，不是错误：翻译层把它当 error 抛过，整张会话列表随之报废。
func TestV3ConversationListSurfacesResetRequiredNotError(t *testing.T) {
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"conversations":[],"done":true,"reset_required":true}`))
	})
	page, err := ctx.V3ConversationList(V3ConversationListReq{UID: "u1"})
	if err != nil {
		t.Fatalf("reset_required 不应该是 error: %v", err)
	}
	if !page.ResetRequired {
		t.Fatal("reset_required 未透传")
	}
}

// 畸形行必须降级进 unresolved，绝不能整页失败——fail-open 是契约。
func TestV3ConversationListDemotesMalformedRowToUnresolved(t *testing.T) {
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"conversations":[
			{"channel_id":"g1","channel_type":2,"unread":1},
			{"channel_id":"bad","channel_type":999}
		],"done":true}`))
	})
	page, err := ctx.V3ConversationList(V3ConversationListReq{UID: "u1"})
	if err != nil {
		t.Fatalf("单行畸形不应让整页失败: %v", err)
	}
	if len(page.Conversations) != 1 || page.Conversations[0].ChannelID != "g1" {
		t.Fatalf("健康会话应照常返回: %+v", page.Conversations)
	}
	if len(page.Unresolved) != 1 || page.Unresolved[0].ChannelID != "bad" {
		t.Fatalf("畸形行应进 unresolved: %+v", page.Unresolved)
	}
}

func TestV3ChannelMessageSyncPassesThroughEventMetaAndMillis(t *testing.T) {
	var gotBody map[string]interface{}
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channel/messagesync" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"start_message_seq":0,"end_message_seq":0,"more":1,"messages":[
			{"header":{"no_persist":0,"red_dot":1,"sync_once":0},"message_id":9,"message_idstr":"9","message_seq":12,
			 "client_msg_no":"m9","from_uid":"u2","channel_id":"u2","channel_type":1,"timestamp":1787556650,
			 "payload":"e30=","event_meta":{"kind":"stream"},"stream_data":"AQ=="}]}`))
	})

	resp, err := ctx.V3ChannelMessageSync(V3ChannelSyncReq{
		LoginUID: "u1", ChannelID: "u2", ChannelType: 1,
		StartMessageSeq: 5, EndMessageSeq: 0, Limit: 20, PullMode: PullModeDown,
		IncludeEventMeta: true,
	})
	if err != nil {
		t.Fatalf("V3ChannelMessageSync: %v", err)
	}
	if gotBody["include_event_meta"] != float64(1) {
		t.Fatalf("include_event_meta 未透传: %+v", gotBody)
	}
	if resp.StartMessageSeq != 5 {
		t.Fatalf("start 应按请求回填（引擎只回声），got %d", resp.StartMessageSeq)
	}
	if resp.More != 1 {
		t.Fatalf("more 未透传")
	}
	msg := resp.Messages[0]
	if msg.TimestampMS != 1787556650000 {
		t.Fatalf("秒应换算成毫秒, got %d", msg.TimestampMS)
	}
	if msg.MessageSeqStr != "12" {
		t.Fatalf("seq 字符串形式缺失: %+v", msg)
	}
	if string(msg.EventMeta) != `{"kind":"stream"}` {
		t.Fatalf("event_meta 应原样透传, got %s", msg.EventMeta)
	}
	if len(msg.StreamData) == 0 {
		t.Fatal("stream_data 应原样透传")
	}
}

// 批量同步里单个频道失败只写进该条目的 Err，其余频道必须照常返回。
func TestV3ChannelMessageSyncBatchIsolatesFailures(t *testing.T) {
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channel/messagesyncbatch" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"channel_id":"g1","channel_type":2,"messages":[{"message_idstr":"1","message_seq":1,"timestamp":1787556650}]},
			{"channel_id":"g2","channel_type":2,"error":"membership required","messages":[]}
		]}`))
	})
	results, err := ctx.V3ChannelMessageSyncBatch("u1", []V3ChannelSyncReq{
		{ChannelID: "g1", ChannelType: 2, Limit: 10},
		{ChannelID: "g2", ChannelType: 2, Limit: 10},
	})
	if err != nil {
		t.Fatalf("单频道失败不应让整批失败: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("应逐项对齐返回, got %d", len(results))
	}
	if results[0].Err != nil || len(results[0].Messages) != 1 {
		t.Fatalf("健康频道应照常返回: %+v", results[0])
	}
	if results[1].Err == nil {
		t.Fatal("失败频道应带 Err")
	}
}

// 两次「整群一条消息都看不到」都是隐藏水位高过频道真实 max。守卫必须把它按 0 处理。
func TestV3VisibilityFloorGuardsPoisonedWatermark(t *testing.T) {
	cases := []struct {
		name                          string
		engineFloor, mysqlFloor, tail uint64
		want                          uint64
	}{
		{"取两者较大", 10, 25, 100, 25},
		{"引擎水位更高", 40, 25, 100, 40},
		{"MySQL 水位越界被判毒化", 10, 500, 100, 0},
		{"引擎水位越界被判毒化", 500, 10, 100, 0},
		// 严格大于才算毒化。刚清完屏、或刚以隐藏历史身份入群且此后无新消息时，
		// 水位就正好等于频道 max；判成毒化会把用户清掉的记录和入群前的历史全翻出来。
		{"恰好等于频道 max 是正常状态不是毒化", 100, 0, 100, 100},
		{"清屏到当前末尾后无新消息", 0, 686, 686, 686},
		{"只超出一格也算毒化", 0, 101, 100, 0},
		{"频道 max 未知则不做守卫", 500, 0, 0, 500},
		{"都为零", 0, 0, 100, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := V3VisibilityFloor(tc.engineFloor, tc.mysqlFloor, tc.tail); got != tc.want {
				t.Fatalf("V3VisibilityFloor(%d,%d,%d) = %d, want %d",
					tc.engineFloor, tc.mysqlFloor, tc.tail, got, tc.want)
			}
		})
	}
}

// payload 在引擎那边是 Go 的 []byte，JSON 里是 **base64 字符串**。
// 用 json.RawMessage 接会拿到带引号的 `"eyJ0eXBlIjox..."`，调用方再 Unmarshal 必然失败，
// 每一条消息都落到「消息解析异常」占位——症状是整个会话列表预览全变错误占位，
// 而服务端状态码一切正常。
//
// 原来的几条用例只断言了 seq/时间/游标，payload 一律用 "e30="（空对象）且从不检查内容，
// 所以这个 bug 能带着全绿的测试上线。这条用例专门盯住**解码后的正文**。
func TestV3PayloadIsBase64DecodedNotRawJSON(t *testing.T) {
	// {"type":1,"content":"hi"} 的 base64
	const encoded = "eyJ0eXBlIjoxLCJjb250ZW50IjoiaGkifQ=="
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversation/list":
			_, _ = w.Write([]byte(`{"conversations":[{"channel_id":"u2","channel_type":1,
				"last_message":{"message_idstr":"9","message_seq":12,"client_msg_no":"m9",
				"server_timestamp_ms":1787556650006,"payload":"` + encoded + `"}}],"done":true}`))
		case "/channel/messagesync":
			_, _ = w.Write([]byte(`{"messages":[{"message_idstr":"9","message_seq":12,
				"client_msg_no":"m9","timestamp":1787556650,"payload":"` + encoded + `"}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	page, err := ctx.V3ConversationList(V3ConversationListReq{UID: "u1"})
	if err != nil {
		t.Fatalf("V3ConversationList: %v", err)
	}
	assertDecodedPayload(t, "会话末条", page.Conversations[0].LastMessage.Payload)

	resp, err := ctx.V3ChannelMessageSync(V3ChannelSyncReq{
		LoginUID: "u1", ChannelID: "u2", ChannelType: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("V3ChannelMessageSync: %v", err)
	}
	assertDecodedPayload(t, "频道消息", resp.Messages[0].Payload)
}

func assertDecodedPayload(t *testing.T, where string, payload []byte) {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("%s payload 没被 base64 解码，调用方拿到的是 %q（解析必然失败，"+
			"每条消息都会变成「消息解析异常」占位）", where, string(payload))
	}
	if decoded["content"] != "hi" {
		t.Fatalf("%s payload 正文不对: %+v", where, decoded)
	}
}

// stream_data 引擎侧同样是 []byte（base64），而 event_meta / event_sync_hint 是结构体
// （JSON 对象）。三个字段挨在一起，很容易被一视同仁地都写成 RawMessage 或都写成 []byte，
// 两种写法都会坏掉其中一半。
func TestV3StreamDataDecodedButEventMetaStaysRaw(t *testing.T) {
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "AQID" = 字节 0x01 0x02 0x03
		_, _ = w.Write([]byte(`{"messages":[{"message_idstr":"9","message_seq":1,
			"timestamp":1787556650,"payload":"e30=","stream_data":"AQID",
			"event_meta":{"kind":"stream","seq":7}}]}`))
	})
	resp, err := ctx.V3ChannelMessageSync(V3ChannelSyncReq{
		LoginUID: "u1", ChannelID: "u2", ChannelType: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("V3ChannelMessageSync: %v", err)
	}
	msg := resp.Messages[0]
	if !bytes.Equal(msg.StreamData, []byte{1, 2, 3}) {
		t.Fatalf("stream_data 应被 base64 解码成原始字节, got %v", msg.StreamData)
	}
	if string(msg.EventMeta) != `{"kind":"stream","seq":7}` {
		t.Fatalf("event_meta 是对象，应原样透传不解码, got %s", msg.EventMeta)
	}
}

func TestV3NativeRequiresV3Engine(t *testing.T) {
	cfg := New()
	cfg.WuKongIM.Engine = "v2"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	if _, err := ctx.V3ConversationList(V3ConversationListReq{UID: "u1"}); err != ErrV3EngineDisabled {
		t.Fatalf("v2 引擎下应拒绝 /v3 轨, got %v", err)
	}
}

// login_uid 空会让 v3 拼出一个不存在的单聊频道并静默返回空列表——必须在本层拦住。
func TestV3ChannelMessageSyncRejectsEmptyLoginUID(t *testing.T) {
	ctx := newNativeTestContext(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("不应该发出请求")
	})
	if _, err := ctx.V3ChannelMessageSync(V3ChannelSyncReq{ChannelID: "u2", ChannelType: 1}); err == nil {
		t.Fatal("空 login_uid 应报错")
	}
}
