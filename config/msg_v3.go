package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jianyu-im/JianYuServerLib/common"
	"github.com/jianyu-im/JianYuServerLib/pkg/network"
	"github.com/jianyu-im/JianYuServerLib/pkg/util"
	"go.uber.org/zap"
)

// WuKongIM v3 适配层。
//
// v3 相比 v2 移除了下列后端在用的接口，这里用 v3 现有能力把语义翻译回去，
// 保证 server 与客户端的对外契约完全不变：
//
//	/conversation/sync        -> /conversation/list （游标翻页 + 本地按 version 过滤）
//	/conversations            -> /conversation/list
//	/channel/max_message_seq  -> /channel/messagesync（PullModeDown 取最新一条）
//	/messages                 -> /channel/messagesync（按 seq 区间拉取后过滤）
//	/message/sendbatch        -> 逐个 /message/send
//
// 只有 cfg.WuKongIM.APIVersion == "v3" 时才会走到这里；默认仍是 v2 原路径，
// 线上连 v2 引擎的节点不受影响。
//
// 关于 version：v2 的会话 version 语义就是「会话最后活跃时间戳」
// （见 server 端 SyncUserConversationReq.Version 的注释），
// 而 v3 每个会话都带 active_at，两者可以直接对齐，无需伪造版本号。

// IMV3Enabled 是否对接 v3 引擎
func (c *Context) IMV3Enabled() bool {
	return strings.EqualFold(strings.TrimSpace(c.cfg.WuKongIM.APIVersion), "v3")
}

// ---------- v3 原始响应结构 ----------

type v3ConversationLastMessage struct {
	MessageID         uint64 `json:"message_id"`
	MessageIDStr      string `json:"message_idstr"`
	MessageSeq        uint64 `json:"message_seq"`
	FromUID           string `json:"from_uid"`
	ClientMsgNo       string `json:"client_msg_no"`
	ServerTimestampMS int64  `json:"server_timestamp_ms"`
	Payload           []byte `json:"payload"`
}

type v3ConversationItem struct {
	ChannelID    string                     `json:"channel_id"`
	ChannelType  int64                      `json:"channel_type"`
	ActiveAt     int64                      `json:"active_at"`
	ReadSeq      uint64                     `json:"read_seq"`
	DeletedToSeq uint64                     `json:"deleted_to_seq"`
	Unread       uint64                     `json:"unread"`
	LastMessage  *v3ConversationLastMessage `json:"last_message"`
}

type v3ConversationListResp struct {
	Conversations []*v3ConversationItem `json:"conversations"`
	NextCursor    string                `json:"next_cursor"`
	Done          bool                  `json:"done"`
	Coverage      int64                 `json:"coverage"`
	ResetRequired bool                  `json:"reset_required"`
}

// ---------- 迁移占位消息的末条回退 ----------

// seqPadClientMsgNoPrefixes 迁移/重建时灌的占位消息（type=99）的 client_msg_no 前缀。
//
// 简语 2026-08-24 停机重建用 wkmigrate 生成的占位是 "hole-"（补洞 + 顶格占位到 seqCeil，
// payload 为 {"type":99,"cmd":"jy_seq_hole"}），zeroim 迁移用 pad_seq.py 灌的是 "seqpad-"。
// 两者都对用户不可见：安卓 SDK 六处过滤 WK_INSIDE_MSG=99，PC 内核把 99 建模为 cmd，都不渲染。
var seqPadClientMsgNoPrefixes = []string{"hole-", "seqpad-"}

// isSeqPadMessage 判断是不是补 seq 的占位消息
func isSeqPadMessage(clientMsgNo string) bool {
	for _, prefix := range seqPadClientMsgNoPrefixes {
		if strings.HasPrefix(clientMsgNo, prefix) {
			return true
		}
	}
	return false
}

// lastRealMessageCacheTTL 占位消息是灌进去的静态历史，只要频道没来新消息，
// 「末尾 seq → 往前第一条真实消息」这个映射就不会变，缓存久一点也不会脏。
const lastRealMessageCacheTTL = 30 * time.Minute

// lastRealMessageScanLimit 单轮往回翻的条数，最多翻 lastRealMessageScanRounds 轮。
// 占位消息是「从原 max seq 补到目标 seq」的一整段连续区间，zeroim 实测有频道连续五十条全是占位，
// 一轮 50 条跨不过去，所以按 200 拉（v3 messagesync 实测接受，返回不足则说明已到频道头部）。
const (
	lastRealMessageScanLimit  = 200
	lastRealMessageScanRounds = 3
)

type lastRealMessageCacheItem struct {
	msg *v3ConversationLastMessage
	at  time.Time
}

// lastRealMessageCache key: channelType|channelID|末尾seq
var lastRealMessageCache sync.Map

// resolveConversationLastMessageV3 把「会话最后一条是补 seq 占位消息」的情况回退成往前第一条真实消息。
//
// v3 的 /conversation/list 每个会话只给频道末尾那一条，而重建时的顶格占位往往正好是末尾。
// 客户端拿到的就是一条它永远渲染不出来的消息：会话预览停在空内容上，聊天页的「未读 N ↓」跳到最新
// 也看不到任何变化（用户报的「点了没反应」）。
//
// 注意不能简单把 last_message 置空，只做替换，找不到真实消息时保持原样。
// 命中占位才会多打一次 messagesync，且结果按 (频道, 末尾seq) 缓存，稳态下几乎不产生额外请求。
func (c *Context) resolveConversationLastMessageV3(loginUID, channelID string, channelType uint8, last *v3ConversationLastMessage) *v3ConversationLastMessage {
	if last == nil {
		// 会话对账 activate 出来的骨架行（membership 里没有对当前用户可见的消息）last_message 为 nil。
		// 群聊以用户身份从频道尾部回填最近一条真实消息当预览；用户对它是否可见由下游按
		// visibles/offset 再过滤，这里不做策略判断。单聊保持 nil，避免复活用户已删除的会话。
		if channelType != common.ChannelTypeGroup.Uint8() {
			return nil
		}
		return c.resolveWithCacheV3(loginUID, channelID, channelType, nil, 0)
	}
	if !isSeqPadMessage(last.ClientMsgNo) {
		return last
	}
	return c.resolveWithCacheV3(loginUID, channelID, channelType, last, last.MessageSeq)
}

// resolveWithCacheV3 按 (频道, 末尾seq) 缓存地扫描「往前第一条真实消息」。
// tailSeq=0 表示末条未知（骨架会话），从频道最新一屏开始找。
func (c *Context) resolveWithCacheV3(loginUID, channelID string, channelType uint8, last *v3ConversationLastMessage, tailSeq uint64) *v3ConversationLastMessage {
	cacheKey := fmt.Sprintf("%d|%s|%d", channelType, channelID, tailSeq)
	if v, ok := lastRealMessageCache.Load(cacheKey); ok {
		if item, ok := v.(lastRealMessageCacheItem); ok && time.Since(item.at) < lastRealMessageCacheTTL {
			if item.msg == nil {
				return last
			}
			return item.msg
		}
	}
	resolved := c.scanLastRealMessageV3(loginUID, channelID, channelType, tailSeq)
	lastRealMessageCache.Store(cacheKey, lastRealMessageCacheItem{msg: resolved, at: time.Now()})
	if resolved == nil {
		return last
	}
	return resolved
}

// scanLastRealMessageV3 从末尾往旧翻，找第一条非占位消息。
// tailSeq=0 表示从频道最新一屏开始（v3 messagesync start=0 + PullModeDown 返回末尾窗口）。
func (c *Context) scanLastRealMessageV3(loginUID, channelID string, channelType uint8, tailSeq uint64) *v3ConversationLastMessage {
	if loginUID == "" {
		return nil
	}
	cursor := uint32(tailSeq)
	for round := 0; round < lastRealMessageScanRounds; round++ {
		if cursor == 0 && round > 0 {
			return nil
		}
		syncResp, err := c.imV3SyncChannelMessages(loginUID, channelID, channelType,
			cursor, 0, lastRealMessageScanLimit, PullModeDown)
		if err != nil {
			c.Warn("v3会话最后一条消息回退失败", zap.String("channel_id", channelID), zap.Error(err))
			return nil
		}
		if syncResp == nil || len(syncResp.Messages) == 0 {
			return nil
		}
		var minSeq uint32
		for _, m := range syncResp.Messages {
			if m == nil || m.MessageSeq == 0 {
				continue
			}
			if minSeq == 0 || m.MessageSeq < minSeq {
				minSeq = m.MessageSeq
			}
		}
		// messagesync 返回顺序不保证，自己按 seq 从大到小挑第一条真实消息
		var best *MessageResp
		for _, m := range syncResp.Messages {
			if m == nil || isSeqPadMessage(m.ClientMsgNo) || m.MessageSeq == 0 {
				continue
			}
			if tailSeq > 0 && uint64(m.MessageSeq) > tailSeq {
				continue
			}
			if best == nil || m.MessageSeq > best.MessageSeq {
				best = m
			}
		}
		if best != nil {
			return &v3ConversationLastMessage{
				MessageID:         uint64(best.MessageID),
				MessageIDStr:      best.MessageIDStr,
				MessageSeq:        uint64(best.MessageSeq),
				FromUID:           best.FromUID,
				ClientMsgNo:       best.ClientMsgNo,
				ServerTimestampMS: int64(best.Timestamp) * 1000,
				Payload:           best.Payload,
			}
		}
		if minSeq <= 1 {
			return nil
		}
		cursor = minSeq - 1
	}
	return nil
}

// v3ConversationPageLimit 单页拉取条数。
// v3 的 /conversation/list 硬性要求 limit <= 200（internal/usecase/conversation
// 的 maxListLimit），超过直接返回 invalid request，所以这里取上限 200。
const v3ConversationPageLimit = 200

// v3RecentsFetchLimit 一次会话同步里最多给多少个会话补拉多条历史（其余只给 last_message）。
const v3RecentsFetchLimit = 30

// v3MaxConversationPages 翻页上限，防止服务端游标异常时死循环。
const v3MaxConversationPages = 200

// ---------- /conversation/list 的闸门与缓存 ----------
//
// 2026-08-24 线上事故：v3 的 /conversation/list 在 IM 侧要为**每一个**会话回读最后一条消息
// （pkg/cluster/channels.(*Service).ReadConversationHeads → ListMessagesBySeq → readRowsReverse），
// 单次调用的内存代价跟「该用户的会话数」成正比。而这里是全量翻页拉取、没有增量水位，
// 早高峰几百个用户同时做会话同步，就把 WuKongIM 的堆顶到 16G（其中 12.5G 全在 ReadConversationHeads），
// 被内核 OOM kill，进而重启循环、全站掉线。
//
// 所以在调用方这一侧加两道闸：
//  1. 同一个 uid 的结果短时间内复用，挡住客户端重连/多端/重试打出来的重复全量拉取；
//  2. 全局并发上限，保证任何时刻只有少数几个全量拉取压在 IM 上，其余排队而不是一起冲。
const (
	convListCacheTTL     = 45 * time.Second // 会话列表复用窗口
	convListMaxInflight  = 24               // 同时压在 IM 上的全量拉取数
	convListWaitDeadline = 20 * time.Second // 排队等不到就用过期缓存兜底
)

type convListCacheItem struct {
	items []*v3ConversationItem
	at    time.Time
}

var (
	convListCache    sync.Map // uid -> convListCacheItem
	convListInflight = make(chan struct{}, convListMaxInflight)
)

// loadConvListCache 取缓存，onlyFresh=false 时连过期的也返回（排队超时的兜底用）
func loadConvListCache(uid string, onlyFresh bool) ([]*v3ConversationItem, bool) {
	v, ok := convListCache.Load(uid)
	if !ok {
		return nil, false
	}
	item, ok := v.(convListCacheItem)
	if !ok {
		return nil, false
	}
	if onlyFresh && time.Since(item.at) >= convListCacheTTL {
		return nil, false
	}
	return item.items, true
}

// imV3ListConversations 翻页拉完某用户的全部会话（带复用窗口与并发闸门）
func (c *Context) imV3ListConversations(uid string) ([]*v3ConversationItem, error) {
	if items, ok := loadConvListCache(uid, true); ok {
		return items, nil
	}
	timer := time.NewTimer(convListWaitDeadline)
	defer timer.Stop()
	select {
	case convListInflight <- struct{}{}:
		defer func() { <-convListInflight }()
	case <-timer.C:
		// IM 正忙，宁可给一份稍旧的会话列表，也不要再加一个全量拉取把它压垮
		if items, ok := loadConvListCache(uid, false); ok {
			c.Warn("v3会话列表排队超时，返回上一次结果", zap.String("uid", uid))
			return items, nil
		}
		// 一次缓存都没有：这是用户第一次同步，返回错误等于「会话页空白」，
		// 体验上不可接受。宁可越过闸门直接拉一次，也不能让人看不到会话列表。
		c.Warn("v3会话列表排队超时且无缓存，越过闸门直取", zap.String("uid", uid))
		items, err := c.imV3ListConversationsDirect(uid)
		if err != nil {
			return nil, err
		}
		convListCache.Store(uid, convListCacheItem{items: items, at: time.Now()})
		return items, nil
	}
	// 排队期间可能已被别的请求填好
	if items, ok := loadConvListCache(uid, true); ok {
		return items, nil
	}
	items, err := c.imV3ListConversationsDirect(uid)
	if err != nil {
		return nil, err
	}
	convListCache.Store(uid, convListCacheItem{items: items, at: time.Now()})
	return items, nil
}

// imV3ListConversationsDirect 真正去 IM 翻页拉取
func (c *Context) imV3ListConversationsDirect(uid string) ([]*v3ConversationItem, error) {
	all := make([]*v3ConversationItem, 0, 64)
	cursor := ""
	for page := 0; page < v3MaxConversationPages; page++ {
		body := map[string]interface{}{
			"uid":   uid,
			"limit": v3ConversationPageLimit,
		}
		if cursor != "" {
			body["cursor"] = cursor
		}
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversation/list", []byte(util.ToJson(body)), nil)
		if err != nil {
			return nil, err
		}
		if err = c.handlerIMError(resp); err != nil {
			return nil, err
		}
		var page3 v3ConversationListResp
		if err = util.ReadJsonByByte([]byte(resp.Body), &page3); err != nil {
			return nil, err
		}
		all = append(all, page3.Conversations...)
		if page3.Done || page3.NextCursor == "" {
			return all, nil
		}
		cursor = page3.NextCursor
	}
	// 到达翻页上限仍未 done：返回已取到的部分，避免调用方长时间阻塞
	return all, nil
}

// v3LastMessageToMessageResp 把 v3 会话里的最后一条消息转成旧结构
func v3LastMessageToMessageResp(item *v3ConversationItem) *MessageResp {
	if item == nil {
		return nil
	}
	return v3LastMessageRespWith(item.LastMessage, item.ChannelID, uint8(item.ChannelType))
}

// v3LastMessageRespWith 与 v3LastMessageToMessageResp 相同，但允许调用方传入
// 经过占位回退（resolveConversationLastMessageV3）后的最后一条消息
func v3LastMessageRespWith(lm *v3ConversationLastMessage, channelID string, channelType uint8) *MessageResp {
	if lm == nil {
		return nil
	}
	return &MessageResp{
		MessageID:    int64(lm.MessageID),
		MessageIDStr: lm.MessageIDStr,
		MessageSeq:   uint32(lm.MessageSeq),
		ClientMsgNo:  lm.ClientMsgNo,
		FromUID:      lm.FromUID,
		ChannelID:    channelID,
		ChannelType:  channelType,
		// v3 用毫秒，旧结构是 10 位秒级
		Timestamp: int32(lm.ServerTimestampMS / 1000),
		Payload:   append([]byte(nil), lm.Payload...),
	}
}

// v3ConversationTimestamp 取会话时间（秒级）。
//
// v3 的 active_at 来自 UserChannelMembership.ActivatedAt，只有走过
// /conversations/activate 的会话才有值；用 /channel/subscriber_add 灌进去的
// 成员关系该字段是 0。而 v2 的会话 version/timestamp 语义是「最后活跃时间」，
// 因此取 active_at 与最后一条消息时间两者较大值，保证增量同步的水位线可用。
func v3ConversationTimestamp(item *v3ConversationItem) int64 {
	if item == nil {
		return 0
	}
	var lastMessageMS int64
	if item.LastMessage != nil {
		lastMessageMS = item.LastMessage.ServerTimestampMS
	}
	return conversationTimestampV3(item.ActiveAt, lastMessageMS)
}

// conversationTimestampV3 计算下发给客户端的会话时间（秒），语义对齐 v2 的「最后一次会话时间」。
func conversationTimestampV3(activeAt int64, lastMessageMS int64) int64 {
	ts := normalizeEpochMSV3(activeAt)
	if lm := normalizeEpochMSV3(lastMessageMS); lm > ts {
		ts = lm
	}
	if ts <= 0 {
		return 0
	}
	return ts / 1000
}

// normalizeEpochMSV3 把量纲不明的 epoch 时间归一化到毫秒。
//
// v3 的 active_at 实际是 wukongim ActivateConversation 写入的 time.Now().UnixNano()（纳秒，
// internal/usecase/conversation/unread.go），而迁移导入的行可能是 0/秒/毫秒；若按毫秒 ÷1000 下发，
// 凡被会话对账 activate 过的会话，客户端会拿到 16 位「微秒」时间戳，被永远钉在列表顶部
// （zeroim 2026-08-24 现场）。按数量级判定：秒 ~2e9、毫秒 ~2e12、微秒 ~2e15、纳秒 ~2e18，
// 各量纲间隔千倍，边界取 1e11/1e14/1e17 在 5138 年前都不会误判。
func normalizeEpochMSV3(v int64) int64 {
	switch {
	case v <= 0:
		return 0
	case v < 1e11: // 秒
		return v * 1000
	case v < 1e14: // 毫秒
		return v
	case v < 1e17: // 微秒
		return v / 1e3
	default: // 纳秒
		return v / 1e6
	}
}

// imSyncUserConversationV3 会话同步（对应 v2 的 /conversation/sync）
//
// v3 的 /conversation/list 不支持按 version 增量，这里拉全量后在本地按
// active_at > version 过滤。结果按 active_at 升序返回，因为调用方会取
// 最后一条的 Version 当作新的水位线存起来。
func (c *Context) imSyncUserConversationV3(uid string, version int64, msgCount int64, expectedGroups []*Channel) ([]*SyncUserConversationResp, error) {
	items, err := c.imV3ListConversations(uid)
	if err != nil {
		return nil, err
	}
	// 群目录对账必须 fail-open：一个修不好的群只该缺它自己，绝不能拖垮整张会话列表。
	// zeroim 2026-08-24 的 fail-closed 版本（对账出错或修完仍缺就整体 return error）线上表现为：
	// 单个 uid 的 conversation/sync 恒定失败、客户端 3 秒一 retry、iOS 建连流程永远完不成
	// （用户报「无法长连接」），当天两次上线两次紧急回滚。缺行留给下一次 sync 继续补。
	activated, reconcileErr := c.reconcileExpectedGroupConversationsV3(uid, expectedGroups, items)
	if reconcileErr != nil {
		c.Warn("v3群会话对账未完成，按现有目录降级返回", zap.String("uid", uid), zap.Error(reconcileErr))
	}
	if activated {
		// 激活成功后必须重新读取 WuKongIM 的权威投影，不能在适配层手工拼骨架会话；
		// 这样 last_message/unread/可见性仍全部由 IM 的 membership + hydration 合同决定。
		// 注意要绕过 45s 复用缓存直取，并把新结果回填缓存，否则读到的还是对账前的旧列表。
		refreshed, rerr := c.imV3ListConversationsDirect(uid)
		if rerr != nil {
			c.Warn("v3群会话对账后重读目录失败，沿用对账前结果", zap.String("uid", uid), zap.Error(rerr))
		} else {
			convListCache.Store(uid, convListCacheItem{items: refreshed, at: time.Now()})
			items = refreshed
			if missing := missingExpectedGroupConversationsV3(expectedGroups, items); len(missing) > 0 {
				missingNos := make([]string, 0, len(missing))
				for _, m := range missing {
					missingNos = append(missingNos, m.ChannelID)
				}
				c.Warn("v3群会话对账后目录仍缺群，降级返回现有会话", zap.String("uid", uid), zap.Strings("group_nos", missingNos))
			}
		}
	}
	result := make([]*SyncUserConversationResp, 0, len(items))
	for _, item := range items {
		if item == nil || item.ChannelID == "" {
			continue
		}
		// 末条是重建占位消息时回退成往前第一条真实消息，否则客户端预览是一条渲染不出来的空消息
		lastMessage := c.resolveConversationLastMessageV3(uid, item.ChannelID, uint8(item.ChannelType), item.LastMessage)
		var lastMessageMS int64
		if lastMessage != nil {
			lastMessageMS = lastMessage.ServerTimestampMS
		}
		activeAt := conversationTimestampV3(item.ActiveAt, lastMessageMS)
		// 这里刻意【不做】version 增量过滤，全量回传。
		//
		// v2 的 /conversation/sync 由 IM 引擎按 version 做服务端增量，而 v3 的
		// /conversation/list 只有游标分页、没有版本水位线，过滤只能在本地做。
		// 但客户端的 version 来自它自己的历史同步记录，一旦客户端 version 比
		// 服务端数据新（迁移、换库、回滚等场景），本地过滤会把所有会话滤光，
		// 客户端表现为"一条最近会话都没有"。全量回传由客户端自行 merge 更安全，
		// 会话条数量级(百级)也不构成负担。
		_ = version
		conv := &SyncUserConversationResp{
			ChannelID:    item.ChannelID,
			ChannelType:  uint8(item.ChannelType),
			Unread:       int(item.Unread),
			Timestamp:    activeAt,
			OffsetMsgSeq: int64(item.DeletedToSeq),
			Version:      activeAt,
			Recents:      make([]*MessageResp, 0, 1),
		}
		if lm := v3LastMessageRespWith(lastMessage, item.ChannelID, uint8(item.ChannelType)); lm != nil {
			conv.LastMsgSeq = int64(lastMessage.MessageSeq)
			conv.LastClientMsgNo = lastMessage.ClientMsgNo
			if msgCount > 0 {
				conv.Recents = append(conv.Recents, lm)
			}
		}
		result = append(result, conv)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	// 需要多条最近消息时按频道补拉；msgCount<=1 时 last_message 已足够。
	//
	// 补拉是一个会话一次 /channel/messagesync，会话多的用户一次同步就是几百次 IM 调用，
	// 和 /conversation/list 一起把 IM 的堆顶爆（2026-08-24 事故）。所以只给排在最后的
	// v3RecentsFetchLimit 个（即最近活跃的）会话补拉，其余保留 last_message 那一条——
	// 客户端翻到旧会话时本来也会按频道单独拉历史。
	if msgCount > 1 {
		start := 0
		if len(result) > v3RecentsFetchLimit {
			start = len(result) - v3RecentsFetchLimit
		}
		for _, conv := range result[start:] {
			if conv.LastMsgSeq <= 0 {
				continue
			}
			recents, rerr := c.imV3SyncChannelMessages(uid, conv.ChannelID, conv.ChannelType,
				uint32(conv.LastMsgSeq), 0, int(msgCount), PullModeDown)
			if rerr == nil && recents != nil && len(recents.Messages) > 0 {
				// 补拉窗口里也可能混着重建占位消息（历史段去重换占位），过滤掉；
				// 全被过滤时保留原来的 last_message 那一条，别把 Recents 清空
				filtered := make([]*MessageResp, 0, len(recents.Messages))
				for _, m := range recents.Messages {
					if m == nil || isSeqPadMessage(m.ClientMsgNo) {
						continue
					}
					filtered = append(filtered, m)
				}
				if len(filtered) > 0 {
					conv.Recents = filtered
				}
			}
		}
	}
	return result, nil
}

// imGetConversationsV3 获取用户会话列表（对应 v2 的 /conversations）
func (c *Context) imGetConversationsV3(uid string) ([]*ConversationResp, error) {
	items, err := c.imV3ListConversations(uid)
	if err != nil {
		return nil, err
	}
	result := make([]*ConversationResp, 0, len(items))
	for _, item := range items {
		if item == nil || item.ChannelID == "" {
			continue
		}
		lastMessage := c.resolveConversationLastMessageV3(uid, item.ChannelID, uint8(item.ChannelType), item.LastMessage)
		var lastMessageMS int64
		if lastMessage != nil {
			lastMessageMS = lastMessage.ServerTimestampMS
		}
		result = append(result, &ConversationResp{
			ChannelID:   item.ChannelID,
			ChannelType: uint8(item.ChannelType),
			Unread:      int64(item.Unread),
			Timestamp:   conversationTimestampV3(item.ActiveAt, lastMessageMS),
			LastMessage: v3LastMessageRespWith(lastMessage, item.ChannelID, uint8(item.ChannelType)),
		})
	}
	return result, nil
}

// ---------- 群会话目录对账（v3 UID-owned membership 校准） ----------

// groupConversationActivateWorkers 限制一次批量拉人/对账时对 v3 会话激活接口的并发。
// 激活是幂等的；部分成功后重试整批不会制造重复会话。
const groupConversationActivateWorkers = 8

// groupConversationRepairMaxPerSync 单次会话同步最多修复的缺失群会话数（其余留给后续 sync 分批补齐）。
// 群特别多的账号（管理号可能几百个）首次对账不至于串出几百次 IM HTTP 调用把 conversation/sync 拖到客户端超时。
const groupConversationRepairMaxPerSync = 32

func conversationKeyV3(channelID string, channelType int64) string {
	return channelID + "\x00" + strconv.FormatInt(channelType, 10)
}

// reconcileExpectedGroupConversationsV3 把业务库权威的在册群集合与 WuKongIM 的
// UID-owned 会话目录比对，幂等地补建缺失的行。返回是否有激活动作发生；
// 错误交由调用方降级处理（fail-open），下一次 sync 重读目录后只重试仍缺的群。
func (c *Context) reconcileExpectedGroupConversationsV3(uid string, expected []*Channel, items []*v3ConversationItem) (bool, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" || len(expected) == 0 {
		return false, nil
	}
	missing := missingExpectedGroupConversationsV3(expected, items)
	if len(missing) == 0 {
		return false, nil
	}
	if len(missing) > groupConversationRepairMaxPerSync {
		c.Warn("v3群会话对账缺口超过单次修复上限，分批修复", zap.String("uid", uid), zap.Int("missing", len(missing)), zap.Int("cap", groupConversationRepairMaxPerSync))
		missing = missing[:groupConversationRepairMaxPerSync]
	}

	workerCount := groupConversationActivateWorkers
	if workerCount > len(missing) {
		workerCount = len(missing)
	}
	jobs := make(chan Channel)
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	for index := 0; index < workerCount; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for group := range jobs {
				if err := c.repairGroupConversationV3(uid, group); err != nil {
					firstErrOnce.Do(func() {
						firstErr = fmt.Errorf("uid=%s group_no=%s: %w", uid, group.ChannelID, err)
					})
				}
			}
		}()
	}
	for _, group := range missing {
		jobs <- group
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return false, firstErr
	}
	return true, nil
}

func missingExpectedGroupConversationsV3(expected []*Channel, items []*v3ConversationItem) []Channel {
	existing := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		existing[conversationKeyV3(item.ChannelID, item.ChannelType)] = struct{}{}
	}
	missing := make([]Channel, 0)
	seen := make(map[string]struct{}, len(expected))
	for _, channel := range expected {
		if channel == nil || channel.ChannelType != common.ChannelTypeGroup.Uint8() {
			continue
		}
		groupNo := strings.TrimSpace(channel.ChannelID)
		if groupNo == "" {
			continue
		}
		key := conversationKeyV3(groupNo, int64(channel.ChannelType))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, ok := existing[key]; !ok {
			missing = append(missing, Channel{
				ChannelID:      groupNo,
				ChannelType:    common.ChannelTypeGroup.Uint8(),
				HistoryVisible: channel.HistoryVisible,
			})
		}
	}
	return missing
}

// repairGroupConversationV3 优先保留既有 membership 的入群水位：只有明确 not found
// 时才走订阅者投影（按业务群的历史可见策略补建缺失的 UID-owned 行），再重试激活。
func (c *Context) repairGroupConversationV3(uid string, group Channel) error {
	err := c.activateConversationV3(uid, group.ChannelID, group.ChannelType)
	if err == nil {
		return nil
	}
	if !isMissingConversationMembershipV3(err) {
		return err
	}
	if err := c.projectGroupMembershipV3(uid, group); err != nil {
		return fmt.Errorf("project missing membership: %w", err)
	}
	if err := c.activateConversationV3(uid, group.ChannelID, group.ChannelType); err != nil {
		return fmt.Errorf("activate projected membership: %w", err)
	}
	return nil
}

func isMissingConversationMembershipV3(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not found")
}

func (c *Context) projectGroupMembershipV3(uid string, group Channel) error {
	historyVisible := 0
	if group.HistoryVisible == 1 {
		historyVisible = 1
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/subscriber_add", []byte(util.ToJson(map[string]interface{}{
		"channel_id":      group.ChannelID,
		"channel_type":    group.ChannelType,
		"history_visible": historyVisible,
		"subscribers":     []string{uid},
	})), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// activateGroupMemberConversationsV3 显式建立新成员的群会话目录项。
//
// v3 的 /conversation/list 读取的是 UID-owned membership。群 subscriber 与这张目录不是
// 同一个权威集合，不能再让「是否出现群」依赖一条系统提示消息的副作用。
func (c *Context) activateGroupMemberConversationsV3(groupNo string, members []*UserBaseVo) error {
	groupNo = strings.TrimSpace(groupNo)
	if groupNo == "" {
		return fmt.Errorf("group_no不能为空")
	}
	uids := make([]string, 0, len(members))
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		if member == nil {
			continue
		}
		uid := strings.TrimSpace(member.UID)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		uids = append(uids, uid)
	}
	if len(uids) == 0 {
		return nil
	}

	workerCount := groupConversationActivateWorkers
	if workerCount > len(uids) {
		workerCount = len(uids)
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	for index := 0; index < workerCount; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for uid := range jobs {
				err := c.activateConversationV3(uid, groupNo, common.ChannelTypeGroup.Uint8())
				if err != nil {
					firstErrOnce.Do(func() {
						firstErr = fmt.Errorf("uid=%s: %w", uid, err)
					})
				}
			}
		}()
	}
	for _, uid := range uids {
		jobs <- uid
	}
	close(jobs)
	wg.Wait()
	return firstErr
}

func (c *Context) activateConversationV3(uid, channelID string, channelType uint8) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversations/activate", []byte(util.ToJson(map[string]interface{}{
		"uid":          uid,
		"channel_id":   channelID,
		"channel_type": channelType,
	})), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// imV3SyncChannelMessages 调用 v3 的 /channel/messagesync
func (c *Context) imV3SyncChannelMessages(loginUID string, channelID string, channelType uint8,
	startMessageSeq uint32, endMessageSeq uint32, limit int, pullMode PullMode) (*SyncChannelMessageResp, error) {
	if limit <= 0 {
		limit = 100
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesync", []byte(util.ToJson(map[string]interface{}{
		"login_uid":         loginUID,
		"channel_id":        channelID,
		"channel_type":      channelType,
		"start_message_seq": startMessageSeq,
		"end_message_seq":   endMessageSeq,
		"limit":             limit,
		"pull_mode":         pullMode,
	})), nil)
	if err != nil {
		return nil, err
	}
	if err = c.handlerIMError(resp); err != nil {
		// 频道在 v3 里没有时间线（含系统号单聊、无在册成员的废弃群）等价于空结果，
		// 不能让调用方把它当成一次同步失败
		if isChannelAbsentErrV3(err) {
			c.Warn("v3频道无时间线，按空结果返回", zap.String("channel_id", channelID), zap.Uint8("channel_type", channelType), zap.Error(err))
			return emptySyncChannelMessageRespV3(startMessageSeq, endMessageSeq, pullMode), nil
		}
		return nil, err
	}
	var out SyncChannelMessageResp
	if err = util.ReadJsonByByte([]byte(resp.Body), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// imGetChannelMaxSeqV3 取频道最大 seq（对应 v2 的 /channel/max_message_seq）
//
// v3 没有单独的最大序号接口，用 PullModeDown 从最新往回拉一条即可拿到。
func (c *Context) imGetChannelMaxSeqV3(channelID string, channelType uint8) (*ChannelMaxSeqResp, error) {
	resp, err := c.imV3SyncChannelMessages("", channelID, channelType, 0, 0, 1, PullModeDown)
	if err != nil {
		return nil, err
	}
	var maxSeq uint32
	if resp != nil {
		for _, msg := range resp.Messages {
			if msg != nil && msg.MessageSeq > maxSeq {
				maxSeq = msg.MessageSeq
			}
		}
		// 消息为空时退回响应里的区间上界
		if maxSeq == 0 && resp.EndMessageSeq > maxSeq {
			maxSeq = resp.EndMessageSeq
		}
	}
	return &ChannelMaxSeqResp{MessageSeq: maxSeq}, nil
}

// v3MessageFetchMaxSpan 单次按区间补拉的最大跨度，防止稀疏 seq 触发巨量拉取
const v3MessageFetchMaxSpan = 2000

// imGetMessagesBySeqsV3 按 seq 列表取消息（对应 v2 的 /messages）
//
// v3 只能按区间同步，这里取 [min,max] 后在本地过滤出需要的 seq。
func (c *Context) imGetMessagesBySeqsV3(channelID string, channelType uint8, loginUID string,
	seqs []uint32) (*SyncChannelMessageResp, error) {
	if len(seqs) == 0 {
		return &SyncChannelMessageResp{Messages: make([]*MessageResp, 0)}, nil
	}
	want := make(map[uint32]struct{}, len(seqs))
	minSeq, maxSeq := seqs[0], seqs[0]
	for _, s := range seqs {
		want[s] = struct{}{}
		if s < minSeq {
			minSeq = s
		}
		if s > maxSeq {
			maxSeq = s
		}
	}
	if minSeq > 0 {
		minSeq-- // start 是开区间，往前退一位才能覆盖到 minSeq 本身
	}
	span := int(maxSeq-minSeq) + 1
	if span > v3MessageFetchMaxSpan {
		span = v3MessageFetchMaxSpan
	}
	resp, err := c.imV3SyncChannelMessages(loginUID, channelID, channelType, minSeq, maxSeq+1, span, PullModeUp)
	if err != nil {
		return nil, err
	}
	filtered := make([]*MessageResp, 0, len(seqs))
	if resp != nil {
		for _, msg := range resp.Messages {
			if msg == nil {
				continue
			}
			if _, ok := want[msg.MessageSeq]; ok {
				filtered = append(filtered, msg)
			}
		}
	}
	return &SyncChannelMessageResp{
		StartMessageSeq: minSeq,
		EndMessageSeq:   maxSeq,
		PullMode:        PullModeUp,
		Messages:        filtered,
	}, nil
}

// imSearchMessagesV3 消息检索（对应 v2 的 /messages）
//
// v3 只保留了按 seq 区间同步的能力，因此仅支持 MessageSeqs 检索；
// 按 message_id / client_msg_no 检索在 v3 没有对应接口，明确报错而不是静默返回空，
// 避免调用方把「查不到」当成「不存在」。
func (c *Context) imSearchMessagesV3(req *MsgSearchReq) (*SyncChannelMessageResp, error) {
	if req == nil {
		return &SyncChannelMessageResp{Messages: make([]*MessageResp, 0)}, nil
	}
	if len(req.MessageSeqs) > 0 {
		return c.imGetMessagesBySeqsV3(req.ChannelID, req.ChannelType, req.LoginUID, req.MessageSeqs)
	}
	if len(req.MessageIds) > 0 || len(req.ClientMsgNos) > 0 {
		return nil, fmt.Errorf("WuKongIM v3 不支持按 message_id/client_msg_no 检索消息，请改用 message_seqs")
	}
	return &SyncChannelMessageResp{Messages: make([]*MessageResp, 0)}, nil
}

// sendMessageBatchV3 批量发消息（对应 v2 的 /message/sendbatch）
//
// v3 去掉了批量接口，这里逐个订阅者调 /message/send。注意不能用 send 的
// subscribers 字段代替：那是一次性命令频道（request-scoped），不落库，
// 与 sendbatch「给每个人各发一条正常消息」的语义不同。
func (c *Context) sendMessageBatchV3(req *MsgSendBatch) error {
	if req == nil || len(req.Subscribers) == 0 {
		return nil
	}
	var failed []string
	var lastErr error
	for _, uid := range req.Subscribers {
		if uid == "" {
			continue
		}
		// 走统一的 SendMessageWithResult，才能拿到 from_uid 回落和 reason 拒收判定
		// （手工拼 body 直发会把 v3 的 200+reason≠1 当成功，整批静默丢掉）
		err := c.SendMessage(&MsgSendReq{
			Header:      req.Header,
			FromUID:     req.FromUID,
			ChannelID:   uid,
			ChannelType: common.ChannelTypePerson.Uint8(),
			Payload:     req.Payload,
		})
		if err != nil {
			failed = append(failed, uid)
			lastErr = err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("IM服务[SendMessageBatch-v3]部分失败！失败 %d/%d，最后一个错误：%v",
			len(failed), len(req.Subscribers), lastErr)
	}
	return nil
}

// ---------- v3 发送契约适配 ----------

// msgReasonSuccessV3 v3 /message/send 返回体里表示「已收下」的 reason
// （对应上游 pkg/protocol/frame.ReasonSuccess，reason 枚举从 0=Unknown 开始）。
const msgReasonSuccessV3 = 1

// msgReasonTextV3 把 v3 的 reason 码翻成可读名字，只覆盖会打到日志里的那几个，
// 其余原样返回数字（完整枚举见上游 pkg/protocol/frame/common.go 的 ReasonCode）。
func msgReasonTextV3(reason int64) string {
	switch reason {
	case 0:
		return "Unknown"
	case 3:
		return "SubscriberNotExist 发送者不在频道成员里"
	case 4:
		return "InBlacklist 发送者在黑名单里"
	case 5:
		return "ChannelNotExist 频道不存在"
	case 11:
		return "NotAllowSend 不允许发送"
	case 13:
		return "NotInWhitelist 发送者不在频道白名单里"
	case 15:
		return "SystemError"
	case 19:
		return "Ban 频道被封禁"
	case 22:
		return "RateLimit 被限流"
	case 24:
		return "Disband 频道已解散"
	case 25:
		return "SendBan 发送被封禁"
	}
	return fmt.Sprintf("reason:%d", reason)
}

// adaptSendReqV3 把 v2 语义的 /message/send 请求改写成 WuKongIM v3 能接受的形式。
//
// 三处契约差异（对 v3 实测得出）：
//
//  1. from_uid 必填。v2 为空时回落到系统账号（jianyuim internal/api/message.go 的
//     options.G.SystemUID 分支），v3 少了这层回落，一律 400 invalid request。
//     服务端 CMD（清红点、已读回执、在线状态、群头像）和群提示消息都不带 from_uid，
//     于是全挂——建群后连一条 tip 消息都落不下去，群就不会出现在会话列表里。
//
//  2. subscribers 与 channel_id 互斥，且必须 sync_once=1，否则 400。
//
//  3. 单聊频道的 no_persist+sync_once（也就是所有单聊 CMD）在 v3 是坏的：
//     内部先给频道套 ____cmd 后缀、再做人与人频道归一化，发送方按 crc32 排在后位时
//     直接 400 invalid channel id，排在前位时算出的频道又和路由目标对不上 → 503
//     retry required。所以单聊 CMD 一律改走 subscribers 定向投递——那条路径不碰频道归一化。
func adaptSendReqV3(req *MsgSendReq, systemUID string) *MsgSendReq {
	if req == nil {
		return nil
	}
	next := *req
	if strings.TrimSpace(next.FromUID) == "" {
		next.FromUID = systemUID
	}
	// 单聊 CMD 改写成定向投递：收件人就是这条单聊频道的对端
	if len(next.Subscribers) == 0 &&
		next.Header.NoPersist == 1 && next.Header.SyncOnce == 1 &&
		next.ChannelType == common.ChannelTypePerson.Uint8() &&
		strings.TrimSpace(next.ChannelID) != "" {
		if left, right, ok := strings.Cut(next.ChannelID, "@"); ok && left != "" && right != "" {
			// 调用方传了库内 fake id（a@b）。直接当 uid 投递等于发给一个不存在的人，
			// 静默无投递也无报错；拆成两端投递才等价于 v2 的单聊频道语义。
			next.Subscribers = []string{left, right}
		} else {
			next.Subscribers = []string{next.ChannelID}
		}
	}
	if len(next.Subscribers) == 0 {
		return &next
	}
	targets := make([]string, 0, len(next.Subscribers))
	seen := make(map[string]struct{}, len(next.Subscribers))
	for _, uid := range next.Subscribers {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		targets = append(targets, uid)
	}
	next.Subscribers = targets
	if len(targets) == 0 {
		// 只剩空 uid，退回频道投递，别发一条 v3 必然 400 的空 subscribers 请求
		next.Subscribers = nil
		return &next
	}
	next.ChannelID = ""
	next.ChannelType = 0
	next.Header.SyncOnce = 1
	return &next
}

// isChannelAbsentErrV3 判断 v3 的错误是否等价于「这个频道在 v3 里不存在 / 没有可见时间线」。
//
// v3 的 /channel/messagesync 对一条消息都没有的频道返回的是 `valid channel membership required`
// 而不是 `channel not found`——频道不存在就没有成员关系，成员校验先失败。
//
// 线上现象：系统号 u_10000 的单聊、以及没有任何在册成员的废弃群，客户端每次打开都收到
// 「同步频道内的消息失败」，会话直接打不开。对调用方来说这等价于「空时间线」。
func isChannelAbsentErrV3(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "valid channel membership required") ||
		strings.Contains(msg, "channel not found")
}

// emptySyncChannelMessageRespV3 构造与请求同形的空结果，让调用方走「没有更多消息」分支
func emptySyncChannelMessageRespV3(startMessageSeq, endMessageSeq uint32, pullMode PullMode) *SyncChannelMessageResp {
	return &SyncChannelMessageResp{
		StartMessageSeq: startMessageSeq,
		EndMessageSeq:   endMessageSeq,
		PullMode:        pullMode,
		Messages:        make([]*MessageResp, 0),
	}
}

// groupCMDSubscriberBatchSize 群 CMD 定向投递的分批大小。
// v3 的 request-scoped 频道 id 是订阅者列表的哈希，一次带太多 uid 会让请求体过大，
// 分批发既能控制单次体积，也让部分失败不至于整群收不到。
const groupCMDSubscriberBatchSize = 500

// sendGroupCMDV3 把群频道的 CMD 改成按成员定向投递。
//
// v3 里群 CMD 走频道这条路是断的：CMD 写在 "<群号>____cmd" 这条日志上，
// 但投递时是拿这个带后缀的频道 id 去查订阅者的（internal/runtime/channelappend/recipient.go），
// 查不到任何人，于是 HTTP 200、reason 也可能正常，消息却一个人都收不到。
// 而且服务端 CMD 用的是系统账号，系统账号本来就不在群里，群成员校验先一步返回
// ReasonSubscriberNotExist。两个坑叠在一起，群头像/群成员变更的 CMD 全是静默失败。
//
// 定向投递（subscribers）那条路径不做频道成员校验，也不查频道订阅者，正好绕开两者。
// 返回 handled=false 表示不适用（没注入 provider、查不到成员、或该群不走定向投递），
// 调用方按原来的频道方式发送。
func (c *Context) sendGroupCMDV3(req *MsgSendReq) (handled bool, err error) {
	if c.groupMemberProvider == nil {
		return false, nil
	}
	if req == nil || req.ChannelType != common.ChannelTypeGroup.Uint8() || strings.TrimSpace(req.ChannelID) == "" {
		return false, nil
	}
	if len(req.Subscribers) > 0 {
		return false, nil
	}
	members, err := c.groupMemberProvider(req.ChannelID)
	if err != nil {
		c.Error("查询群成员失败，群CMD退回频道投递", zap.String("group_no", req.ChannelID), zap.Error(err))
		return false, nil
	}
	if len(members) == 0 {
		return false, nil
	}
	groupNo := req.ChannelID
	var firstErr error
	for index, batch := range chunkSubscribers(members, groupCMDSubscriberBatchSize) {
		if _, err := c.SendMessageWithResult(&MsgSendReq{
			Header:      req.Header,
			Setting:     req.Setting,
			FromUID:     req.FromUID,
			Subscribers: batch,
			Payload:     req.Payload,
		}); err != nil {
			c.Error("群CMD分批投递失败",
				zap.String("group_no", groupNo),
				zap.Int("batch_index", index),
				zap.Int("batch_size", len(batch)),
				zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return true, firstErr
}

// chunkSubscribers 把订阅者列表按 size 切片，size <= 0 时不切。
func chunkSubscribers(uids []string, size int) [][]string {
	if len(uids) == 0 {
		return nil
	}
	if size <= 0 || len(uids) <= size {
		return [][]string{uids}
	}
	batches := make([][]string, 0, (len(uids)+size-1)/size)
	for start := 0; start < len(uids); start += size {
		end := start + size
		if end > len(uids) {
			end = len(uids)
		}
		batches = append(batches, uids[start:end])
	}
	return batches
}
