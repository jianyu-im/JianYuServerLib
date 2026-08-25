package config

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/common"
	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

// capturedSend 记录一次到达假 IM 服务的 /message/send 请求
type capturedSend struct {
	FromUID     string   `json:"from_uid"`
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	Subscribers []string `json:"subscribers"`
	Header      struct {
		NoPersist int `json:"no_persist"`
		SyncOnce  int `json:"sync_once"`
	} `json:"header"`
}

// newV3ContextForTest 造一个指向假 IM 服务、开着 v3 引擎的 Context
func newV3ContextForTest(t *testing.T) (*Context, *[]capturedSend, *sync.Mutex) {
	t.Helper()

	var mu sync.Mutex
	sends := make([]capturedSend, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got capturedSend
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("解析请求体失败: %v", err)
		}
		mu.Lock()
		sends = append(sends, got)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message_id":1,"message_seq":0,"reason":1}`))
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	cfg.Account.SystemUID = "u_10000"

	return &Context{cfg: cfg, Log: log.NewTLog("test")}, &sends, &mu
}

func TestSendCMDV3GroupFansOutToMembersInBatches(t *testing.T) {
	ctx, sends, mu := newV3ContextForTest(t)
	members := make([]string, 0, groupCMDSubscriberBatchSize+3)
	for i := 0; i < groupCMDSubscriberBatchSize+3; i++ {
		members = append(members, "u_"+string(rune('A'+i%26))+string(rune('a'+i/26)))
	}
	ctx.SetGroupMemberProvider(func(groupNo string) ([]string, error) {
		if groupNo != "g_1" {
			t.Errorf("provider 收到的群号 = %q, want g_1", groupNo)
		}
		return members, nil
	})

	if err := ctx.SendCMD(MsgCMDReq{
		ChannelID:   "g_1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         "groupAvatarUpdate",
		Param:       map[string]interface{}{"group_no": "g_1"},
	}); err != nil {
		t.Fatalf("SendCMD() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*sends) != 2 {
		t.Fatalf("发出请求数 = %d, want 2 批", len(*sends))
	}
	total := 0
	for i, got := range *sends {
		if got.ChannelID != "" || got.ChannelType != 0 {
			t.Fatalf("第 %d 批仍带频道: %+v（v3 里 subscribers 和 channel_id 互斥）", i, got)
		}
		if got.FromUID != "u_10000" {
			t.Fatalf("第 %d 批 from_uid = %q, want 系统账号", i, got.FromUID)
		}
		if got.Header.SyncOnce != 1 || got.Header.NoPersist != 1 {
			t.Fatalf("第 %d 批 header = %+v, want no_persist=1 sync_once=1", i, got.Header)
		}
		total += len(got.Subscribers)
	}
	if total != len(members) {
		t.Fatalf("收件人合计 = %d, want %d（一个都不能漏）", total, len(members))
	}
	if got := (*sends)[0].Subscribers; len(got) != groupCMDSubscriberBatchSize {
		t.Fatalf("第 1 批大小 = %d, want %d", len(got), groupCMDSubscriberBatchSize)
	}
}

func TestSendCMDV3GroupFallsBackToChannelWhenProviderReturnsNothing(t *testing.T) {
	// 超级群等场景 provider 返回空，要退回原来的频道投递，不能把 CMD 吞掉
	ctx, sends, mu := newV3ContextForTest(t)
	ctx.SetGroupMemberProvider(func(string) ([]string, error) { return nil, nil })

	if err := ctx.SendCMD(MsgCMDReq{
		ChannelID:   "g_super",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		CMD:         "groupMemberUpdate",
	}); err != nil {
		t.Fatalf("SendCMD() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*sends) != 1 {
		t.Fatalf("发出请求数 = %d, want 1", len(*sends))
	}
	got := (*sends)[0]
	if got.ChannelID != "g_super" || got.ChannelType != common.ChannelTypeGroup.Uint8() {
		t.Fatalf("退回频道投递时应保留频道: %+v", got)
	}
	if len(got.Subscribers) != 0 {
		t.Fatalf("subscribers = %#v, want 空", got.Subscribers)
	}
}

func TestSendCMDV3PersonStillGoesToPeerOnly(t *testing.T) {
	// 单聊 CMD 不受群 provider 影响，仍然只投给对端
	ctx, sends, mu := newV3ContextForTest(t)
	ctx.SetGroupMemberProvider(func(string) ([]string, error) {
		t.Error("单聊 CMD 不该去查群成员")
		return nil, nil
	})

	if err := ctx.SendCMD(MsgCMDReq{
		ChannelID:   "u_b",
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         "unreadClear",
		Param:       map[string]interface{}{"channel_id": "u_c", "channel_type": 1},
	}); err != nil {
		t.Fatalf("SendCMD() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(*sends) != 1 {
		t.Fatalf("发出请求数 = %d, want 1", len(*sends))
	}
	if got := (*sends)[0].Subscribers; !reflect.DeepEqual(got, []string{"u_b"}) {
		t.Fatalf("subscribers = %#v, want [u_b]", got)
	}
}
