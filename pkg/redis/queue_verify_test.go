package redis

import (
	"fmt"
	"sync"
	"testing"
)

const vKey = "_verify_readedq"

func newTestConn(t *testing.T) *Conn {
	c := New("127.0.0.1:6379", "")
	if _, err := c.Ping(); err != nil {
		t.Skipf("本地无 Redis，跳过: %v", err)
	}
	c.Del(vKey)
	return c
}

// FIFO：先入队的先被取出
func TestQueueFIFO(t *testing.T) {
	c := newTestConn(t)
	defer c.Del(vKey)
	for i := 0; i < 5; i++ {
		if err := c.PushQueue(vKey, 0, fmt.Sprintf("item-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.PopQueueBatch(vKey, 5)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range got {
		want := fmt.Sprintf("item-%d", i)
		if v != want {
			t.Fatalf("FIFO 顺序错: 第%d个是 %q，应为 %q", i, v, want)
		}
	}
	t.Logf("FIFO 正确: %v", got)
}

// 取出即移除，且分批取不重不漏
func TestQueuePopRemoves(t *testing.T) {
	c := newTestConn(t)
	defer c.Del(vKey)
	vals := make([]interface{}, 0, 10)
	for i := 0; i < 10; i++ {
		vals = append(vals, fmt.Sprintf("v%d", i))
	}
	if err := c.PushQueue(vKey, 0, vals...); err != nil {
		t.Fatal(err)
	}
	first, _ := c.PopQueueBatch(vKey, 4)
	second, _ := c.PopQueueBatch(vKey, 4)
	third, _ := c.PopQueueBatch(vKey, 4)
	fourth, _ := c.PopQueueBatch(vKey, 4)
	if len(first) != 4 || len(second) != 4 || len(third) != 2 || len(fourth) != 0 {
		t.Fatalf("分批取数量错: %d/%d/%d/%d，应为 4/4/2/0", len(first), len(second), len(third), len(fourth))
	}
	seen := map[string]bool{}
	for _, batch := range [][]string{first, second, third} {
		for _, v := range batch {
			if seen[v] {
				t.Fatalf("重复取到 %q", v)
			}
			seen[v] = true
		}
	}
	if len(seen) != 10 {
		t.Fatalf("漏取: 只拿到 %d 条，应为 10", len(seen))
	}
	n, _ := c.QueueLen(vKey)
	if n != 0 {
		t.Fatalf("取空后 LLEN 应为 0，实际 %d", n)
	}
	t.Log("取出即移除、分批不重不漏、取空后长度归零")
}

// maxLen 上限：超出时丢弃最旧的，保留最新的
func TestQueueMaxLen(t *testing.T) {
	c := newTestConn(t)
	defer c.Del(vKey)
	for i := 0; i < 20; i++ {
		if err := c.PushQueue(vKey, 5, fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := c.QueueLen(vKey)
	if n != 5 {
		t.Fatalf("maxLen=5 时长度应为 5，实际 %d", n)
	}
	got, _ := c.PopQueueBatch(vKey, 10)
	want := []string{"n15", "n16", "n17", "n18", "n19"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("应保留最新的 5 条 %v，实际 %v", want, got)
		}
	}
	t.Logf("上限生效，保留最新: %v", got)
}

// 空队列返回空切片而不是错误
func TestQueueEmpty(t *testing.T) {
	c := newTestConn(t)
	defer c.Del(vKey)
	got, err := c.PopQueueBatch(vKey, 10)
	if err != nil {
		t.Fatalf("空队列不该报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空队列应返回 0 条，实际 %d", len(got))
	}
	t.Log("空队列返回空切片，无错误")
}

// 并发：多写多读，一条不丢一条不重（这是 Lua 原子性的关键验证）
func TestQueueConcurrent(t *testing.T) {
	c := newTestConn(t)
	defer c.Del(vKey)
	const writers, perWriter = 8, 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := c.PushQueue(vKey, 0, fmt.Sprintf("w%d-i%d", w, i)); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	var mu sync.Mutex
	seen := map[string]int{}
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				batch, err := c.PopQueueBatch(vKey, 37)
				if err != nil {
					t.Error(err)
					return
				}
				if len(batch) == 0 {
					return
				}
				mu.Lock()
				for _, v := range batch {
					seen[v]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	total := writers * perWriter
	if len(seen) != total {
		t.Fatalf("并发取: 拿到 %d 个唯一值，应为 %d（有丢失）", len(seen), total)
	}
	for v, cnt := range seen {
		if cnt != 1 {
			t.Fatalf("并发取: %q 被取到 %d 次（重复）", v, cnt)
		}
	}
	n, _ := c.QueueLen(vKey)
	if n != 0 {
		t.Fatalf("并发取完后应为空，实际 %d", n)
	}
	t.Logf("并发 8写6读 共 %d 条: 无丢失、无重复、取空", total)
}

// ScanKeys 能扫到并遵守 limit
func TestScanKeys(t *testing.T) {
	c := newTestConn(t)
	prefix := "_verify_scan:"
	for i := 0; i < 30; i++ {
		c.SetAndExpire(fmt.Sprintf("%s%d", prefix, i), "x", 60000000000)
	}
	defer func() {
		for i := 0; i < 30; i++ {
			c.Del(fmt.Sprintf("%s%d", prefix, i))
		}
	}()
	all, err := c.ScanKeys(prefix+"*", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 30 {
		t.Fatalf("应扫到 30 个，实际 %d", len(all))
	}
	limited, _ := c.ScanKeys(prefix+"*", 10)
	if len(limited) > 10 {
		t.Fatalf("limit=10 时返回了 %d 个", len(limited))
	}
	t.Logf("SCAN 全量 %d 个，limit=10 返回 %d 个", len(all), len(limited))
}
