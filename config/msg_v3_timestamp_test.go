package config

import "testing"

// 2026-08-24 线上实测值：会话对账 activate 后 wukongim 写入的 active_at 是 UnixNano。
// 此前按毫秒 ÷1000 下发，客户端拿到 16 位「微秒」时间戳被永远钉顶。
func TestNormalizeEpochMSV3(t *testing.T) {
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"零值", 0, 0},
		{"负值", -1, 0},
		{"秒", 1787556650, 1787556650000},
		{"毫秒", 1787559084112, 1787559084112},
		{"微秒", 1787556650006265, 1787556650006},
		{"纳秒(线上activate实测值)", 1787556650006265796, 1787556650006},
	}
	for _, c := range cases {
		if got := normalizeEpochMSV3(c.in); got != c.want {
			t.Errorf("%s: normalizeEpochMSV3(%d) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

func TestConversationTimestampV3MixedUnits(t *testing.T) {
	// activate 的纳秒不再必然压过毫秒的 last_message：两者归一化后取较大者，输出秒
	ns := int64(1787556650006265796)  // 15:30:50
	lastMS := int64(1787559084112)    // 16:11:24，更晚
	if got := conversationTimestampV3(ns, lastMS); got != 1787559084 {
		t.Errorf("conversationTimestampV3(ns, laterMS) = %d, want 1787559084", got)
	}
	// 骨架会话（无 last_message）：时间应是 activate 时刻的秒，不是微秒
	if got := conversationTimestampV3(ns, 0); got != 1787556650 {
		t.Errorf("conversationTimestampV3(ns, 0) = %d, want 1787556650", got)
	}
	if got := conversationTimestampV3(0, 0); got != 0 {
		t.Errorf("conversationTimestampV3(0, 0) = %d, want 0", got)
	}
}
