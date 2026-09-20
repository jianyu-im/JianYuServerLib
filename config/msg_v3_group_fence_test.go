package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jianyu-im/JianYuServerLib/pkg/log"
)

// 模拟引擎的版本栅栏：墓碑版本 tombstoneVersion，频道计数器从 counter 起，每次真实的移出/加入 +1；
// 加入时拿到的版本号 > 墓碑版本才算成员关系生效。
func TestHealGroupMembershipV3BumpsPastFence(t *testing.T) {
	var mu sync.Mutex
	counter := uint64(100)
	const tombstoneVersion = uint64(160)
	subscribed := true // 订阅者行在，但 membership 是墓碑
	member := false
	adds, removes := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/channel/subscriber_add":
			var req struct {
				HistoryVisible int `json:"history_visible"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.HistoryVisible != 1 {
				t.Errorf("history_visible = %d, want 1", req.HistoryVisible)
			}
			adds++
			if !subscribed {
				counter++ // 只有真实变更才涨版本
				subscribed = true
			}
			if counter+1 > tombstoneVersion {
				member = true
			}
			_, _ = w.Write([]byte(`{"status":200}`))
		case "/channel/subscriber_remove":
			removes++
			if subscribed {
				counter++
				subscribed = false
			}
			_, _ = w.Write([]byte(`{"status":200}`))
		case "/channel/messagesync":
			if !member {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"msg":"internal/message: valid channel membership required","status":400}`))
				return
			}
			_, _ = w.Write([]byte(`{"messages":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	groupFenceBumpAttempts.Delete("g1|u1")

	if err := ctx.healGroupMembershipV3("u1", Channel{ChannelID: "g1", ChannelType: 2, HistoryVisible: 1}); err != nil {
		t.Fatalf("healGroupMembershipV3() error = %v", err)
	}
	if !member || !subscribed {
		t.Fatalf("member=%v subscribed=%v, want both true", member, subscribed)
	}
	if removes == 0 || removes > groupFenceBumpMaxCycles {
		t.Fatalf("removes=%d adds=%d", removes, adds)
	}
}

// 计数器已经自然涨过墓碑版本：重新投影一次就够，不应该去顶版本。
func TestHealGroupMembershipV3PlainReprojectIsEnough(t *testing.T) {
	var mu sync.Mutex
	member := false
	removes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/channel/subscriber_add":
			member = true
			_, _ = w.Write([]byte(`{"status":200}`))
		case "/channel/subscriber_remove":
			removes++
			_, _ = w.Write([]byte(`{"status":200}`))
		case "/channel/messagesync":
			if !member {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"msg":"internal/message: valid channel membership required","status":400}`))
				return
			}
			_, _ = w.Write([]byte(`{"messages":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := New()
	cfg.WuKongIM.APIURL = server.URL
	cfg.WuKongIM.Engine = "v3"
	ctx := &Context{cfg: cfg, Log: log.NewTLog("test")}
	if err := ctx.healGroupMembershipV3("u2", Channel{ChannelID: "g2", ChannelType: 2}); err != nil {
		t.Fatalf("error = %v", err)
	}
	if removes != 0 {
		t.Fatalf("removes=%d, want 0", removes)
	}
}
