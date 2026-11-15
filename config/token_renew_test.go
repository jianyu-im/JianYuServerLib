package config

import (
	"os"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
)

func TestTokenRenewerThrottle(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := &tokenRenewer{memo: map[string]time.Time{}, now: func() time.Time { return now }}
	if !r.due("a") || r.due("a") {
		t.Fatal("同一 token 一小时内只应检查一次")
	}
	now = now.Add(renewCheckInterval)
	if !r.due("a") {
		t.Fatal("满一小时后应再次检查")
	}
}

// 需要本地 Redis：TOKEN_RENEW_TEST_REDIS=127.0.0.1:6390 go test ./config -run TokenRenewScript
func TestTokenRenewScript(t *testing.T) {
	addr := os.Getenv("TOKEN_RENEW_TEST_REDIS")
	if addr == "" {
		t.Skip("TOKEN_RENEW_TEST_REDIS 未设置")
	}
	c := rd.NewClient(&rd.Options{Addr: addr})
	defer c.Close()
	c.FlushDB()
	full := int64(30 * 24 * 3600)
	slack := int64(24 * 3600)
	run := func(tok, uid string) int64 {
		v, err := c.Eval(renewScript, []string{"token:" + tok, "usertokens:" + uid}, full, slack, tok, "uidtoken:", uid).Int64()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ttl := func(k string) int64 { return int64(c.TTL(k).Val() / time.Second) }
	day := 24 * time.Hour

	// 登录 28 天后的 token：本体、当前 APP 槽、索引都续回 30 天；同 uid 的网页旧槽不是这个 token，不动
	c.Set("token:t1", "u1-|@Sassn-|@Sass-|@SassX", 2*day)
	c.Set("uidtoken:0u1", "t1", 2*day)
	c.Set("uidtoken:1u1", "other", 2*day)
	c.HSet("usertokens:u1", "t1", "0")
	c.Expire("usertokens:u1", 2*day)
	if v := run("t1", "u1"); v != 1 {
		t.Fatalf("t1 ret=%d", v)
	}
	if ttl("token:t1") < full-5 || ttl("uidtoken:0u1") < full-5 || ttl("usertokens:u1") < full-5 {
		t.Fatalf("未续满: %d %d %d", ttl("token:t1"), ttl("uidtoken:0u1"), ttl("usertokens:u1"))
	}
	if ttl("uidtoken:1u1") > int64(2*day/time.Second) {
		t.Fatal("别的设备槽不应被续")
	}

	// 刚登录（剩余 > 29 天）不动
	c.Set("token:t2", "x", 30*day-time.Hour)
	if v := run("t2", "u2"); v != 0 {
		t.Fatalf("t2 ret=%d", v)
	}
	// 已吊销（不存在）不会被续活
	if v := run("t3", "u3"); v != -1 || c.Exists("token:t3").Val() != 0 {
		t.Fatalf("t3 ret=%d", v)
	}
	// 历史永久 token 不加过期
	c.Set("token:t4", "x", 0)
	if v := run("t4", "u4"); v != -1 || c.TTL("token:t4").Val() != -1*time.Second {
		t.Fatalf("t4 ret=%d ttl=%v", v, c.TTL("token:t4").Val())
	}
}
