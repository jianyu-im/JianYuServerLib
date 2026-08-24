package config

import (
	"reflect"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/common"
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

func TestWithSystemUIDInWhitelist(t *testing.T) {
	for _, tc := range []struct {
		name      string
		uids      []string
		systemUID string
		want      []string
	}{
		{
			// 取消全员禁言就是把白名单清空，这时补系统号会变成「只有系统号能发言」
			name: "空白名单保持为空", uids: nil, systemUID: "u_10000", want: nil,
		},
		{
			name: "禁言白名单补上系统号",
			uids: []string{"u_a", "u_b"}, systemUID: "u_10000",
			want: []string{"u_a", "u_b", "u_10000"},
		},
		{
			name: "已含系统号不重复添加",
			uids: []string{"u_a", "u_10000"}, systemUID: "u_10000",
			want: []string{"u_a", "u_10000"},
		},
		{
			name: "未配置系统号时原样返回",
			uids: []string{"u_a"}, systemUID: "  ",
			want: []string{"u_a"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := withSystemUIDInWhitelist(tc.uids, tc.systemUID); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("withSystemUIDInWhitelist(%v, %q) = %v, want %v", tc.uids, tc.systemUID, got, tc.want)
			}
		})
	}
}

func TestIsChannelAbsentErrV3(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "成员校验失败即频道无时间线", err: errTest("IM服务失败！ -> valid channel membership required"), want: true},
		{name: "频道不存在", err: errTest("channel not found"), want: true},
		{name: "其它错误不降级", err: errTest("rate limit exceeded"), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isChannelAbsentErrV3(tc.err); got != tc.want {
				t.Fatalf("isChannelAbsentErrV3(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
