package config

// 上游 WuKongIM v3（main 分支）适配层。
//
// 多产品共用本库：v2 产品（jianyuim fork）行为完全不变；
// 仅当配置 im.engine=v3 时，msg.go 中对应函数分流到本文件的等价实现。
// v3 与 v2 的接口差异见 customer/docs/wukongim-v3-standalone-deploy.md §6。

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

// imEngineV3 是否使用上游 WuKongIM v3 引擎
func (c *Context) imEngineV3() bool {
	return strings.EqualFold(strings.TrimSpace(c.cfg.WuKongIM.Engine), "v3")
}

// sendMessageBatchV3 v3 无 /message/sendbatch；v3 的 subscribers 定向发送要求 sync_once=1（不落频道
// 时间线），与 v2 sendbatch 落每人单聊频道的语义不同。为保持语义，改为逐人按单聊频道发送。
// 调用方（管理后台群发）本身已做分片+worker池，逐条发送的吞吐可接受
func (c *Context) sendMessageBatchV3(req *MsgSendBatch) error {
	var firstErr error
	for _, uid := range req.Subscribers {
		if uid == "" || uid == req.FromUID {
			continue
		}
		if err := c.SendMessage(&MsgSendReq{
			Header:      req.Header,
			FromUID:     req.FromUID,
			ChannelID:   uid,
			ChannelType: uint8(common.ChannelTypePerson),
			Payload:     req.Payload,
		}); err != nil {
			// 单个接收者失败不中断整批，记录首个错误返回
			c.Error("SendMessageBatch 单条发送失败", zap.String("to_uid", uid), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

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

// conversationListCursor v3 /conversation/list 游标
type conversationListCursor struct {
	ActiveAt    int64  `json:"active_at"`
	ChannelID   string `json:"channel_id"`
	ChannelType int64  `json:"channel_type"`
}

// conversationListLastMessage v3 /conversation/list 里每个会话的最后一条消息
type conversationListLastMessage struct {
	MessageID         uint64 `json:"message_id"`
	MessageIDStr      string `json:"message_idstr"`
	MessageSeq        uint64 `json:"message_seq"`
	FromUID           string `json:"from_uid"`
	ClientMsgNo       string `json:"client_msg_no"`
	ServerTimestampMS int64  `json:"server_timestamp_ms"`
	Payload           []byte `json:"payload"`
}

type conversationListKey struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int64  `json:"channel_type"`
}

type conversationListConversation struct {
	ChannelID   string                       `json:"channel_id"`
	ChannelType int64                        `json:"channel_type"`
	ActiveAt    int64                        `json:"active_at"`
	Unread      uint64                       `json:"unread"`
	LastMessage *conversationListLastMessage `json:"last_message"`
}

// conversationListResp v3 /conversation/list 响应
type conversationListResp struct {
	Conversations []conversationListConversation `json:"conversations"`
	Deletes       []conversationListKey          `json:"deletes"`
	Unresolved    []conversationListKey          `json:"unresolved"`
	NextCursor    string                         `json:"next_cursor"` // v3 为不透明字符串游标（原按 conversationListCursor 对象解析会 unmarshal 崩溃 → 400）
	Done          bool                           `json:"done"`        // v3 用 done 标记同步完成；无 more 字段
	Coverage      int64                          `json:"coverage"`
	ResetRequired bool                           `json:"reset_required"`
}

// seqPadClientMsgNoPrefix 2026-08-23 迁移时 pad_seq.py 灌的占位消息（type=99）的 client_msg_no 前缀。
// 补 seq 是为了让 v3 的 seq 水位追上生产库，占位消息本身对用户不可见：安卓 SDK 六处过滤
// WK_INSIDE_MSG=99，PC 内核把 99 建模为 cmd，都不会渲染。
const seqPadClientMsgNoPrefix = "seqpad-"

// lastRealMessageCacheTTL 占位消息是灌进去的静态历史，只要频道没来新消息，
// 「末尾 seq → 往前第一条真实消息」这个映射就不会变，缓存久一点也不会脏。
const lastRealMessageCacheTTL = 30 * time.Minute

// lastRealMessageScanLimit 单轮往回翻的条数，最多翻 lastRealMessageScanRounds 轮。
// 占位消息是「从生产库原 max seq 补到目标 seq」的一整段连续区间，实测有频道 56~105 五十条全是占位，
// 一轮 50 条跨不过去，所以按 200 拉（v3 messagesync 实测接受，返回不足则说明已到频道头部）。
const (
	lastRealMessageScanLimit  = 200
	lastRealMessageScanRounds = 3
)

type lastRealMessageCacheItem struct {
	msg *conversationListLastMessage
	at  time.Time
}

// lastRealMessageCache key: channelType|channelID|末尾seq
var lastRealMessageCache sync.Map

// isSeqPadMessage 判断是不是补 seq 的占位消息
func isSeqPadMessage(clientMsgNo string) bool {
	return strings.HasPrefix(clientMsgNo, seqPadClientMsgNoPrefix)
}

// resolveConversationLastMessageV3 把「会话最后一条是补 seq 占位消息」的情况回退成往前第一条真实消息。
//
// v3 的 /conversation/list 每个会话只给频道末尾那一条，而迁移补的 130 万条占位消息往往正好是末尾。
// 客户端拿到的就是一条它永远渲染不出来的消息：会话预览停在空内容上，聊天页的「未读 N ↓」跳到最新
// 也看不到任何变化（用户报的「点了没反应」）。
//
// 注意不能简单把 last_message 置空：server-v3 的会话同步对单聊有「recents 为空整条丢弃」的闸门
// （modules/message/api_conversation.go），置空会让单聊会话直接从列表里消失。所以只做替换，
// 找不到真实消息时保持原样。
//
// 命中占位才会多打一次 messagesync，且结果按 (频道, 末尾seq) 缓存，稳态下几乎不产生额外请求。
func (c *Context) resolveConversationLastMessageV3(loginUID, channelID string, channelType uint8, last *conversationListLastMessage) *conversationListLastMessage {
	if last == nil {
		// 会话对账 activate 出来的骨架行（membership 里没有对当前用户可见的消息）last_message 为 nil。
		// 群聊会话空 recents 会原样下发（见 server-v3 api_conversation 的骨架会话放行），客户端就是
		// 一行没有内容的空会话。这里以用户身份从频道尾部回填最近一条真实消息当预览；用户对它
		// 是否可见由 server-v3 下游按 visibles/offset 再过滤，这里不做策略判断。
		// 单聊保持 nil：单聊空 recents 会被下游闸门整条丢弃，回填反而会复活用户已删除的会话。
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
func (c *Context) resolveWithCacheV3(loginUID, channelID string, channelType uint8, last *conversationListLastMessage, tailSeq uint64) *conversationListLastMessage {
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
func (c *Context) scanLastRealMessageV3(loginUID, channelID string, channelType uint8, tailSeq uint64) *conversationListLastMessage {
	if loginUID == "" {
		return nil
	}
	cursor := uint32(tailSeq)
	for round := 0; round < lastRealMessageScanRounds; round++ {
		if cursor == 0 && round > 0 {
			return nil
		}
		syncResp, err := c.IMSyncChannelMessage(SyncChannelMessageReq{
			LoginUID:        loginUID,
			ChannelID:       channelID,
			ChannelType:     channelType,
			StartMessageSeq: cursor,
			EndMessageSeq:   0,
			Limit:           lastRealMessageScanLimit,
			PullMode:        PullModeDown,
		})
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
			if tailSeq > 0 && m.MessageSeq > uint32(tailSeq) {
				continue
			}
			if best == nil || m.MessageSeq > best.MessageSeq {
				best = m
			}
		}
		if best != nil {
			return &conversationListLastMessage{
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

// conversationTimestampV3 计算下发给客户端的会话时间（秒），语义对齐 v2 的「最后一次会话时间」。
//
// v3 的 active_at 是 user_channel_membership.activated_at，只有显式调 /conversations/activate
// 才会写；发消息、导入历史都不会 bump 它。实测线上全部会话 active_at=0，直接换算下发会让
// 客户端会话列表的时间显示成 1970/1/1 并且排序全乱。所以以最后一条消息的时间为准，
// active_at 只在没有可见消息（骨架会话）时兜底，取两者较大值。
func conversationTimestampV3(activeAt int64, lastMessage int64) int64 {
	ts := normalizeEpochMSV3(activeAt)
	if lm := normalizeEpochMSV3(lastMessage); lm > ts {
		ts = lm
	}
	if ts <= 0 {
		return 0
	}
	return ts / 1000
}

// conversationVersionV3 计算下发给客户端的会话数据版本（v2 语义的 version 字段）。
//
// v3 引擎没有会话版本号，此前这里恒下发 0。但 winds PC 内核把会话同步做成了两级版本闸门
// （pc/native/crates/kernel-storage/src/android.rs）：
//   - 批级：batch.version（= 各行 version 的 max）<= 本地 kernel_sync_state 水位 → 整批 StaleSync 丢弃；
//   - 行级：incoming.version < existing.version → 保留旧行。
//
// v2 引擎的版本号是纳秒量纲（≈ 更新时刻的 UnixNano），winds 本地水位停在切换前（实测 1.787e18）。
// 恒 0 的版本号让 winds 切 v3 后的每一次会话同步都被静默判旧丢弃：实时投递漏掉的消息永远
// 不会被同步修复，会话列表冻结在最后一条实时消息上——用户看到的就是「列表预览/时间 与
// 打开的聊天窗口对不上」。安卓/iOS 按 REPLACE 覆盖不看 version，所以只有 winds 复现。
//
// 修法沿用 v2 的纳秒量纲：取 max(active_at, 最后一条消息时间) 归一化到毫秒后 ×1e6。
// 频道一有新消息，版本号即单调越过任何 v2 旧水位（同秒内也带毫秒小数位），两级闸门都能通过；
// 无新消息时版本不变，winds 按「已是最新」跳过，语义正确。
func conversationVersionV3(activeAt, lastMessage int64) int64 {
	ms := normalizeEpochMSV3(activeAt)
	if lm := normalizeEpochMSV3(lastMessage); lm > ms {
		ms = lm
	}
	if ms <= 0 {
		return 0
	}
	return ms * int64(time.Millisecond)
}

// normalizeEpochMSV3 把量纲不明的 epoch 时间归一化到毫秒。
//
// v3 的 active_at 实际是 wukongim ActivateConversation 写入的 time.Now().UnixNano()（纳秒，
// internal/usecase/conversation/unread.go），而迁移导入的行是 0；此前按毫秒 ÷1000 下发，
// 凡被会话对账 activate 过的会话，客户端会拿到 16 位「微秒」时间戳，被永远钉在列表顶部。
// 按数量级判定：秒 ~2e9、毫秒 ~2e12、微秒 ~2e15、纳秒 ~2e18，各量纲间隔千倍，边界取 1e11/1e14/1e17
// 在 5138 年前都不会误判。
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

const (
	conversationListPageSizeV3 = 200
	conversationListMaxPagesV3 = 100
)

// listConversationRowsV3 completes one bounded directory pass and resolves the
// retryable rows before exposing the result to the legacy v2-shaped callers.
// Returning a partial page as success makes old clients acknowledge an empty
// sync permanently, so malformed pagination and unresolved rows fail closed.
func (c *Context) listConversationRowsV3(uid string) ([]conversationListConversation, error) {
	rows := make([]conversationListConversation, 0)
	seenRows := make(map[string]struct{})
	unresolved := make(map[string]conversationListKey)
	var cursor string
	completed := false
	for page := 0; page < conversationListMaxPagesV3; page++ {
		reqMap := map[string]interface{}{"uid": uid, "limit": conversationListPageSizeV3}
		if cursor != "" {
			reqMap["cursor"] = cursor
		}
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversation/list", []byte(util.ToJson(reqMap)), nil)
		if err != nil {
			return nil, err
		}
		if err := c.handlerIMError(resp); err != nil {
			return nil, err
		}
		var pageResp conversationListResp
		if err := util.ReadJsonByByte([]byte(resp.Body), &pageResp); err != nil {
			return nil, err
		}
		if pageResp.ResetRequired {
			return nil, fmt.Errorf("v3 conversation directory requires reset for uid %s", uid)
		}
		appendConversationRowsV3(&rows, seenRows, pageResp.Conversations)
		for _, key := range pageResp.Unresolved {
			unresolved[conversationKeyV3(key.ChannelID, key.ChannelType)] = key
		}
		if pageResp.Done {
			completed = true
			break
		}
		if pageResp.NextCursor == "" || pageResp.NextCursor == cursor {
			return nil, fmt.Errorf("v3 conversation directory returned incomplete pagination for uid %s", uid)
		}
		cursor = pageResp.NextCursor
	}
	if !completed {
		return nil, fmt.Errorf("v3 conversation directory exceeded %d pages for uid %s", conversationListMaxPagesV3, uid)
	}
	if len(unresolved) == 0 {
		return rows, nil
	}

	keys := make([]conversationListKey, 0, len(unresolved))
	for _, key := range unresolved {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ChannelID == keys[j].ChannelID {
			return keys[i].ChannelType < keys[j].ChannelType
		}
		return keys[i].ChannelID < keys[j].ChannelID
	})
	for start := 0; start < len(keys); start += conversationListPageSizeV3 {
		end := min(start+conversationListPageSizeV3, len(keys))
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversation/retry", []byte(util.ToJson(map[string]interface{}{
			"uid": uid, "channels": keys[start:end],
		})), nil)
		if err != nil {
			return nil, err
		}
		if err := c.handlerIMError(resp); err != nil {
			return nil, err
		}
		var retryResp conversationListResp
		if err := util.ReadJsonByByte([]byte(resp.Body), &retryResp); err != nil {
			return nil, err
		}
		if retryResp.ResetRequired || len(retryResp.Unresolved) != 0 {
			return nil, fmt.Errorf("v3 conversation directory still has %d unresolved rows for uid %s", len(retryResp.Unresolved), uid)
		}
		appendConversationRowsV3(&rows, seenRows, retryResp.Conversations)
	}
	return rows, nil
}

func appendConversationRowsV3(dst *[]conversationListConversation, seen map[string]struct{}, rows []conversationListConversation) {
	for _, row := range rows {
		key := conversationKeyV3(row.ChannelID, row.ChannelType)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		*dst = append(*dst, row)
	}
}

func conversationKeyV3(channelID string, channelType int64) string {
	return channelID + "\x00" + strconv.FormatInt(channelType, 10)
}

// imGetConversationsV3 v3 无 GET /conversations，用 POST /conversation/list（游标分页）等价聚合，
// 输出结构保持 v2 的 ConversationResp 不变
func (c *Context) imGetConversationsV3(uid string) ([]*ConversationResp, error) {
	rows, err := c.listConversationRowsV3(uid)
	if err != nil {
		return nil, err
	}
	results := make([]*ConversationResp, 0, len(rows))
	for _, conv := range rows {
		lastMessage := c.resolveConversationLastMessageV3(uid, conv.ChannelID, uint8(conv.ChannelType), conv.LastMessage)
		var lastMessageMS int64
		if lastMessage != nil {
			lastMessageMS = lastMessage.ServerTimestampMS
		}
		item := &ConversationResp{
			ChannelID:   conv.ChannelID,
			ChannelType: uint8(conv.ChannelType),
			Unread:      int64(conv.Unread),
			Timestamp:   conversationTimestampV3(conv.ActiveAt, lastMessageMS),
		}
		if lastMessage != nil {
			item.LastMessage = &MessageResp{
				MessageID:    int64(lastMessage.MessageID),
				MessageIDStr: lastMessage.MessageIDStr,
				MessageSeq:   uint32(lastMessage.MessageSeq),
				ClientMsgNo:  lastMessage.ClientMsgNo,
				FromUID:      lastMessage.FromUID,
				ChannelID:    conv.ChannelID,
				ChannelType:  uint8(conv.ChannelType),
				Timestamp:    int32(lastMessage.ServerTimestampMS / 1000),
				Payload:      lastMessage.Payload,
			}
		}
		results = append(results, item)
	}
	return results, nil
}

// imGetChannelMaxSeqV3 v3 无 GET /channel/max_message_seq，用 messagesync 拉最新一条消息推导
// （v3 中 start=0 且 end=0 表示从最新往回读）。
// v3 的 messagesync 要求 login_uid 非空（个人频道用它做归一化）
func (c *Context) imGetChannelMaxSeqV3(channelID string, channelType uint8, loginUID string) (*ChannelMaxSeqResp, error) {
	if loginUID == "" {
		if channelType == uint8(common.ChannelTypePerson) {
			// 个人频道归一化必须要真实的 loginUID，占位值会拼出错误频道
			return nil, fmt.Errorf("v3引擎下查询个人频道最大序号需使用 IMGetChannelMaxSeqWithLoginUID")
		}
		loginUID = "____engine_probe" // 非个人频道仅需非空占位
	}
	syncResp, err := c.IMSyncChannelMessage(SyncChannelMessageReq{
		LoginUID:        loginUID,
		ChannelID:       channelID,
		ChannelType:     channelType,
		StartMessageSeq: 0,
		EndMessageSeq:   0,
		Limit:           1,
		PullMode:        PullModeDown,
	})
	if err != nil {
		return nil, err
	}
	result := &ChannelMaxSeqResp{MessageSeq: 0}
	if syncResp != nil {
		for _, m := range syncResp.Messages {
			if m != nil && m.MessageSeq > result.MessageSeq {
				result.MessageSeq = m.MessageSeq
			}
		}
	}
	return result, nil
}

// imGetWithChannelAndSeqsV3 v3 无 POST /messages（按 seq 列表查询），
// 改用 messagesync 按连续段范围拉取后过滤（start 含界，end 排他）。
// 调用方（引用消息/置顶消息）的 seq 数量少，请求次数 = 连续段数
func (c *Context) imGetWithChannelAndSeqsV3(channelID string, channelType uint8, loginUID string, seqs []uint32) (*SyncChannelMessageResp, error) {
	result := &SyncChannelMessageResp{Messages: make([]*MessageResp, 0, len(seqs))}
	if len(seqs) == 0 {
		return result, nil
	}
	// 去重升序
	sorted := make([]uint32, 0, len(seqs))
	seen := make(map[uint32]struct{}, len(seqs))
	for _, seq := range seqs {
		if seq == 0 {
			continue
		}
		if _, ok := seen[seq]; !ok {
			seen[seq] = struct{}{}
			sorted = append(sorted, seq)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// 连续段分组，逐段 messagesync
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		startSeq, endSeq := sorted[i], sorted[j]
		syncResp, err := c.IMSyncChannelMessage(SyncChannelMessageReq{
			LoginUID:        loginUID,
			ChannelID:       channelID,
			ChannelType:     channelType,
			StartMessageSeq: startSeq,
			EndMessageSeq:   endSeq + 1,
			Limit:           int(endSeq - startSeq + 1),
			PullMode:        PullModeUp,
		})
		if err != nil {
			return nil, err
		}
		if syncResp != nil {
			for _, m := range syncResp.Messages {
				if m == nil {
					continue
				}
				if _, ok := seen[m.MessageSeq]; ok {
					result.Messages = append(result.Messages, m)
				}
			}
		}
		i = j + 1
	}
	return result, nil
}

// imSearchUserMessagesV3 v3 暂无 wk.plugin.search 搜索插件，降级返回空结果
// （全文搜索走服务端自建索引 ZincSearch）
func (c *Context) imSearchUserMessagesV3(req *SearchUserMessageReq) (*SearchUserMessageResp, error) {
	c.Warn("IMSearchUserMessages：WuKongIM v3 暂无搜索插件，返回空结果")
	return &SearchUserMessageResp{
		Total:    0,
		Limit:    req.Limit,
		Page:     req.Page,
		Messages: make([]*MessageResp, 0),
	}, nil
}

// imSyncUserConversationV3 v3 无 POST /conversation/sync（上游 main 在 2026-07 之后移除，实测返回 404），
// 改用 POST /conversation/list 游标分页全量聚合。
//
// 与 v2 的语义差异（调用方已能容忍，见 modules/message/api_conversation.go 对空 Recents 的处理）：
//   - version 增量：v3 没有会话版本号，参数被忽略，每次返回全量会话；返回的 Version 由
//     conversationVersionV3 从会话时间合成（v2 纳秒量纲）——恒 0 会让 winds 的两级版本闸门
//     把每次同步整批判旧丢弃（会话列表冻结、与聊天窗口对不上），不能回退成 0。
//   - Recents：v2 会带回每个会话最近 msgCount 条消息；v3 的 list 每个会话只给 last_message，
//     这里只填 1 条。逐会话再调 messagesync 补齐会把一次同步放大成 N 次 HTTP，得不偿失；
//     客户端进入会话后本来就会走 /message/channel/sync 拉历史。
//   - lastMsgSeqs：目录投影延迟时作为旧客户端已知单聊的有界恢复锚点；不会据此合成消息。
//   - expectedGroups：v2 的 larges 参数在 v3 由 server-v3 传入业务库中的全部有效群。它不是
//     客户端缓存，而是目录校准集合：全新安装及历史异常用户都可据此补齐缺失的 UID-owned membership。
func (c *Context) imSyncUserConversationV3(uid string, msgCount int64, lastMsgSeqs string, expectedGroups []*Channel) ([]*SyncUserConversationResp, error) {
	rows, err := c.listConversationRowsV3(uid)
	if err != nil {
		return nil, err
	}
	// 群目录对账必须 fail-open：一个修不好的群只该缺它自己，绝不能拖垮整张会话列表。
	// 2026-08-24 的 fail-closed 版本（对账出错或修完仍缺就整体 return error）线上表现为：
	// 单个 uid 的 conversation/sync 恒定失败、客户端 3 秒一 retry、iOS 建连流程永远完不成
	// （用户报「无法长连接」），当天两次上线两次紧急回滚。缺行留给下一次 sync 继续补。
	activated, reconcileErr := c.reconcileExpectedGroupConversationsV3(uid, expectedGroups, rows)
	if reconcileErr != nil {
		c.Warn("v3群会话对账未完成，按现有目录降级返回", zap.String("uid", uid), zap.Error(reconcileErr))
	}
	if activated {
		// 激活成功后必须重新读取 WuKongIM 的权威投影，不能在适配层手工拼骨架会话；
		// 这样 last_message/unread/可见性仍全部由 IM 的 membership + hydration 合同决定。
		refreshed, err := c.listConversationRowsV3(uid)
		if err != nil {
			c.Warn("v3群会话对账后重读目录失败，沿用对账前结果", zap.String("uid", uid), zap.Error(err))
		} else {
			rows = refreshed
			if missing := missingExpectedGroupConversationsV3(expectedGroups, rows); len(missing) > 0 {
				missingNos := make([]string, 0, len(missing))
				for _, m := range missing {
					missingNos = append(missingNos, m.ChannelID)
				}
				c.Warn("v3群会话对账后目录仍缺群，降级返回现有会话", zap.String("uid", uid), zap.Strings("group_nos", missingNos))
			}
		}
	}
	results := make([]*SyncUserConversationResp, 0, len(rows))
	for _, conv := range rows {
		lastMessage := c.resolveConversationLastMessageV3(uid, conv.ChannelID, uint8(conv.ChannelType), conv.LastMessage)
		var lastMessageMS int64
		if lastMessage != nil {
			lastMessageMS = lastMessage.ServerTimestampMS
		}
		item := &SyncUserConversationResp{
			ChannelID:   conv.ChannelID,
			ChannelType: uint8(conv.ChannelType),
			Unread:      int(conv.Unread),
			Timestamp:   conversationTimestampV3(conv.ActiveAt, lastMessageMS),
			Version:     conversationVersionV3(conv.ActiveAt, lastMessageMS),
			Recents:     make([]*MessageResp, 0, 1),
		}
		if lastMessage != nil {
			item.LastMsgSeq = int64(lastMessage.MessageSeq)
			item.LastClientMsgNo = lastMessage.ClientMsgNo
			if msgCount != 0 {
				item.Recents = append(item.Recents, &MessageResp{
					MessageID:    int64(lastMessage.MessageID),
					MessageIDStr: lastMessage.MessageIDStr,
					MessageSeq:   uint32(lastMessage.MessageSeq),
					ClientMsgNo:  lastMessage.ClientMsgNo,
					FromUID:      lastMessage.FromUID,
					ChannelID:    conv.ChannelID,
					ChannelType:  uint8(conv.ChannelType),
					Timestamp:    int32(lastMessage.ServerTimestampMS / 1000),
					Payload:      lastMessage.Payload,
				})
			}
		}
		results = append(results, item)
	}
	c.backfillUnreadConversationRecentsV3(uid, int(msgCount), results)
	return c.backfillKnownPersonConversationsV3(uid, lastMsgSeqs, int(msgCount), results)
}

// v3RecentsBackfillMax 单次会话同步给未读会话补拉历史的会话数上限。
//
// v2 的 /conversation/sync 会给每个有变更的会话带回 msgCount 条最近消息，断线期间漏收的
// 消息全靠它带回；v3 适配层此前每个会话只带 last_message 一条——安卓端不会主动补中间的洞
// （离线消息监听器只处理 CMD 队列、开聊天页也不保证回拉），用户表现为「对方发了五条只能
// 看见一两条」（2026-08-25 简语现场，服务端单方面修复、客户端零升级）。上限防止单个用户
// 把一次同步放大成几百次 IM 调用（messagesync 已是窗口化读取，单次成本低）。
const v3RecentsBackfillMax = 100

// backfillUnreadConversationRecentsV3 给 unread>0 的会话补拉最近 msgCount 条消息，
// 按最近活跃优先、总量封顶 v3RecentsBackfillMax；补拉失败或全是占位消息时保留原有的
// last_message 那一条，绝不清空 Recents（单聊空 Recents 会被下游闸门整条丢弃）。
func (c *Context) backfillUnreadConversationRecentsV3(uid string, msgCount int, results []*SyncUserConversationResp) {
	if msgCount <= 1 || len(results) == 0 {
		return
	}
	candidates := make([]*SyncUserConversationResp, 0, len(results))
	for _, conv := range results {
		if conv != nil && conv.Unread > 0 && conv.LastMsgSeq > 0 {
			candidates = append(candidates, conv)
		}
	}
	if len(candidates) == 0 {
		return
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Timestamp > candidates[j].Timestamp })
	if len(candidates) > v3RecentsBackfillMax {
		c.Warn("v3会话同步补拉达到上限，部分未读会话只带末条", zap.String("uid", uid), zap.Int("unread_convs", len(candidates)), zap.Int("cap", v3RecentsBackfillMax))
		candidates = candidates[:v3RecentsBackfillMax]
	}
	for _, conv := range candidates {
		recents, err := c.IMSyncChannelMessage(SyncChannelMessageReq{
			LoginUID:        uid,
			ChannelID:       conv.ChannelID,
			ChannelType:     conv.ChannelType,
			StartMessageSeq: uint32(conv.LastMsgSeq),
			EndMessageSeq:   0,
			Limit:           msgCount,
			PullMode:        PullModeDown,
		})
		if err != nil || recents == nil || len(recents.Messages) == 0 {
			continue
		}
		// 补拉窗口里也可能混着迁移占位消息（seqpad），过滤掉；全被过滤时保留原 Recents
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

// reconcileExpectedGroupConversationsV3 compares the business-authoritative
// active group set with WuKongIM's UID-owned directory and idempotently creates
// only missing rows. A partial failure fails the sync closed; the next sync
// rereads the directory and retries only the still-missing groups.
func (c *Context) reconcileExpectedGroupConversationsV3(uid string, expected []*Channel, rows []conversationListConversation) (bool, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" || len(expected) == 0 {
		return false, nil
	}
	missing := missingExpectedGroupConversationsV3(expected, rows)
	if len(missing) == 0 {
		return false, nil
	}
	// 单次 sync 的修复量设上限：群特别多的账号（管理号可能几百个）首次对账不至于
	// 串出几百次 IM HTTP 调用把 conversation/sync 拖到客户端超时；剩余的下次 sync 继续补。
	if len(missing) > groupConversationRepairMaxPerSync {
		c.Warn("v3群会话对账缺口超过单次修复上限，分批修复", zap.String("uid", uid), zap.Int("missing", len(missing)), zap.Int("cap", groupConversationRepairMaxPerSync))
		missing = missing[:groupConversationRepairMaxPerSync]
	}

	workerCount := min(groupConversationActivateWorkers, len(missing))
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

func missingExpectedGroupConversationsV3(expected []*Channel, rows []conversationListConversation) []Channel {
	existing := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		existing[conversationKeyV3(row.ChannelID, row.ChannelType)] = struct{}{}
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

// repairGroupConversationV3 修复目录缺行的群会话。
//
// 关键事实（2026-08-25 简语线上实测）：v3 的 /conversations/activate 是 raft 异步提案，
// FSM apply 撞 ErrNotFound 不会回传，HTTP 永远 200——「activate 成败」不能作为
// membership 是否存在的判据（旧实现靠它分流，坏例永远修不上）。所以先用一次
// 裸 messagesync 探测真实状态，按结果决定投影范围，最后 activate 只做提优先级。
func (c *Context) repairGroupConversationV3(uid string, group Channel) error {
	probeErr := c.rawMessageSyncProbeV3(uid, group.ChannelID, group.ChannelType)
	switch {
	case probeErr == nil:
		// membership 在，只是目录读没带出来（或 activated_at 缺）：提优先级即可
	case isMembershipRequiredErrV3(probeErr):
		// 频道在、本人缺行：投影本人
		if err := c.projectGroupMembersV3([]string{uid}, group); err != nil {
			return fmt.Errorf("project missing membership: %w", err)
		}
	case strings.Contains(probeErr.Error(), "channel not found"):
		// 频道在 v3 里整个不存在（迁移漏建/契约期损失）：尽量投影全员，
		// 只投本人会造出一个单人订阅的群，别人发消息进不来
		members := []string{uid}
		if c.groupMemberProvider != nil {
			if all, err := c.groupMemberProvider(group.ChannelID); err == nil && len(all) > 0 {
				members = all
			}
		}
		c.Warn("v3群频道不存在，按业务成员整体重建", zap.String("group_no", group.ChannelID), zap.Int("members", len(members)))
		if err := c.projectGroupMembersV3(members, group); err != nil {
			return fmt.Errorf("rebuild missing channel: %w", err)
		}
		// 投影只建成员关系，消息日志要靠第一条 append 才存在；无日志的频道会被
		// 会话水合当「已删除」丢弃，群永远进不了列表（2026-08-25 简语 SY079 现场）。
		// 补一条不可见的持久化种子消息（type=99，三端都不渲染）把日志立起来。
		if err := c.seedChannelLogV3(group.ChannelID, group.ChannelType); err != nil {
			return fmt.Errorf("seed rebuilt channel log: %w", err)
		}
	default:
		return probeErr
	}
	if err := c.activateConversationV3(uid, group.ChannelID, group.ChannelType); err != nil {
		return fmt.Errorf("activate membership: %w", err)
	}
	return nil
}

// rawMessageSyncProbeV3 裸调 messagesync 探测频道/成员关系状态，不经过任何
// 「频道不存在→空时间线」的伪装（IMSyncChannelMessage 会伪装，不能用于判断）。
func (c *Context) rawMessageSyncProbeV3(loginUID, channelID string, channelType uint8) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesync", []byte(util.ToJson(map[string]interface{}{
		"login_uid":         loginUID,
		"channel_id":        channelID,
		"channel_type":      channelType,
		"start_message_seq": 0,
		"end_message_seq":   0,
		"limit":             1,
		"pull_mode":         PullModeDown,
	})), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// seedChannelLogV3 给重建出来的空频道补一条不可见的持久化种子消息（type=99，
// 客户端过滤不渲染、red_dot=0 不计未读），让消息日志与会话水合成立。
func (c *Context) seedChannelLogV3(channelID string, channelType uint8) error {
	payload := []byte(util.ToJson(map[string]interface{}{
		"type":  99,
		"cmd":   "jy_channel_seed",
		"param": map[string]interface{}{},
	}))
	_, err := c.SendMessageWithResult(&MsgSendReq{
		Header:      MsgHeader{NoPersist: 0, RedDot: 0, SyncOnce: 0},
		FromUID:     c.cfg.Account.SystemUID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Payload:     payload,
	})
	return err
}

func (c *Context) projectGroupMembersV3(uids []string, group Channel) error {
	historyVisible := 0
	if group.HistoryVisible == 1 {
		historyVisible = 1
	}
	var firstErr error
	for _, batch := range chunkSubscribers(uids, groupCMDSubscriberBatchSize) {
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/subscriber_add", []byte(util.ToJson(map[string]interface{}{
			"channel_id":      group.ChannelID,
			"channel_type":    group.ChannelType,
			"history_visible": historyVisible,
			"subscribers":     batch,
		})), nil)
		if err == nil {
			err = c.handlerIMError(resp)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

const (
	conversationBackfillBatchSizeV3 = 200
	conversationBackfillMinLimitV3  = 50
	conversationBackfillMaxLimitV3  = 200
)

type conversationSyncAnchorV3 struct {
	ChannelID   string
	ChannelType uint8
	LastSeq     uint32
}

type channelMessageSyncBatchItemV3 struct {
	ChannelID       string   `json:"channel_id"`
	ChannelType     uint8    `json:"channel_type"`
	StartMessageSeq uint32   `json:"start_message_seq"`
	EndMessageSeq   uint32   `json:"end_message_seq"`
	Limit           int      `json:"limit"`
	PullMode        PullMode `json:"pull_mode"`
}

type channelMessageSyncBatchResponseV3 struct {
	Items []struct {
		ChannelID   string         `json:"channel_id"`
		ChannelType uint8          `json:"channel_type"`
		Messages    []*MessageResp `json:"messages"`
		Error       string         `json:"error"`
	} `json:"items"`
}

// backfillKnownPersonConversationsV3 recovers only client-known personal
// conversations missing from the membership-backed directory. Message bytes
// still come from WuKongIM's authoritative channel log; last_msg_seqs merely
// bounds the candidate set and supplies the client's local read anchor.
func (c *Context) backfillKnownPersonConversationsV3(uid, lastMsgSeqs string, msgCount int, results []*SyncUserConversationResp) ([]*SyncUserConversationResp, error) {
	anchors := parseConversationSyncAnchorsV3(lastMsgSeqs)
	if len(anchors) == 0 {
		return results, nil
	}
	existing := make(map[string]struct{}, len(results))
	for _, conversation := range results {
		if conversation != nil {
			existing[conversationKeyV3(conversation.ChannelID, int64(conversation.ChannelType))] = struct{}{}
		}
	}
	missing := make([]conversationSyncAnchorV3, 0, len(anchors))
	for _, anchor := range anchors {
		if anchor.ChannelType != common.ChannelTypePerson.Uint8() || anchor.ChannelID == "" || anchor.ChannelID == uid {
			continue
		}
		if _, ok := existing[conversationKeyV3(anchor.ChannelID, int64(anchor.ChannelType))]; ok {
			continue
		}
		missing = append(missing, anchor)
	}
	if len(missing) == 0 {
		return results, nil
	}

	limit := msgCountForConversationBackfillV3(msgCount)
	for start := 0; start < len(missing); start += conversationBackfillBatchSizeV3 {
		end := min(start+conversationBackfillBatchSizeV3, len(missing))
		items := make([]channelMessageSyncBatchItemV3, end-start)
		for index, anchor := range missing[start:end] {
			items[index] = channelMessageSyncBatchItemV3{
				ChannelID: anchor.ChannelID, ChannelType: anchor.ChannelType,
				Limit: limit, PullMode: PullModeDown,
			}
		}
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesyncbatch", []byte(util.ToJson(map[string]interface{}{
			"login_uid": uid, "items": items,
		})), nil)
		if err != nil {
			return nil, err
		}
		if err := c.handlerIMError(resp); err != nil {
			return nil, err
		}
		var batchResp channelMessageSyncBatchResponseV3
		if err := util.ReadJsonByByte([]byte(resp.Body), &batchResp); err != nil {
			return nil, err
		}
		if len(batchResp.Items) != len(items) {
			return nil, fmt.Errorf("v3 person conversation backfill returned %d items, want %d", len(batchResp.Items), len(items))
		}
		for index, item := range batchResp.Items {
			anchor := missing[start+index]
			if item.Error != "" {
				return nil, fmt.Errorf("v3 person conversation backfill failed for %s: %s", anchor.ChannelID, item.Error)
			}
			if conversation := conversationFromBackfillMessagesV3(uid, anchor, item.Messages); conversation != nil {
				results = append(results, conversation)
			}
		}
	}
	return results, nil
}

func msgCountForConversationBackfillV3(requested int) int {
	if requested < conversationBackfillMinLimitV3 {
		return conversationBackfillMinLimitV3
	}
	if requested > conversationBackfillMaxLimitV3 {
		return conversationBackfillMaxLimitV3
	}
	return requested
}

func parseConversationSyncAnchorsV3(value string) []conversationSyncAnchorV3 {
	parts := strings.Split(value, "|")
	anchors := make([]conversationSyncAnchorV3, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		lastColon := strings.LastIndex(part, ":")
		if lastColon <= 0 || lastColon == len(part)-1 {
			continue
		}
		secondColon := strings.LastIndex(part[:lastColon], ":")
		if secondColon <= 0 || secondColon == lastColon-1 {
			continue
		}
		channelID := strings.TrimSpace(part[:secondColon])
		channelType, typeErr := strconv.ParseUint(part[secondColon+1:lastColon], 10, 8)
		lastSeq, seqErr := strconv.ParseUint(part[lastColon+1:], 10, 32)
		if channelID == "" || typeErr != nil || seqErr != nil || channelType == 0 {
			continue
		}
		key := conversationKeyV3(channelID, int64(channelType))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		anchors = append(anchors, conversationSyncAnchorV3{
			ChannelID: channelID, ChannelType: uint8(channelType), LastSeq: uint32(lastSeq),
		})
	}
	return anchors
}

func conversationFromBackfillMessagesV3(uid string, anchor conversationSyncAnchorV3, messages []*MessageResp) *SyncUserConversationResp {
	recents := make([]*MessageResp, 0, len(messages))
	var latest *MessageResp
	unread := 0
	for _, message := range messages {
		if message == nil || message.MessageSeq == 0 || isSeqPadMessage(message.ClientMsgNo) {
			continue
		}
		message.ChannelID = anchor.ChannelID
		message.ChannelType = anchor.ChannelType
		if message.ToUID == "" {
			if message.FromUID == uid {
				message.ToUID = anchor.ChannelID
			} else {
				message.ToUID = uid
			}
		}
		recents = append(recents, message)
		if latest == nil || message.MessageSeq > latest.MessageSeq {
			latest = message
		}
		if message.MessageSeq > anchor.LastSeq && message.FromUID != uid {
			unread++
		}
	}
	if latest == nil {
		return nil
	}
	sort.Slice(recents, func(i, j int) bool { return recents[i].MessageSeq < recents[j].MessageSeq })
	return &SyncUserConversationResp{
		ChannelID: anchor.ChannelID, ChannelType: anchor.ChannelType,
		Unread: unread, Timestamp: int64(latest.Timestamp), LastMsgSeq: int64(latest.MessageSeq),
		LastClientMsgNo: latest.ClientMsgNo, Version: conversationVersionV3(0, int64(latest.Timestamp)*1000), Recents: recents,
	}
}

// adaptSendReqV3 把 v2 语义的 /message/send 请求改写成 WuKongIM v3 能接受的形式。
//
// 2026-08-24 对 121.41.107.95:5001 实测出的三处契约差异：
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
//     retry required。实测拿 12 个真实 uid 打，10 个 400、2 个 503、0 个成功。
//     所以单聊 CMD 一律改走 subscribers 定向投递——那条路径不碰频道归一化。
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
		if left, right, ok := strings.Cut(next.ChannelID, "@"); ok {
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
// 而不是 `channel not found`——频道不存在就没有成员关系，成员校验先失败。迁移期补 seq 水位时
// 踩过同一个坑（见 migrate/pad_seq.py 对单聊的特判）。
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

func isMembershipRequiredErrV3(err error) bool {
	return err != nil && strings.Contains(err.Error(), "valid channel membership required")
}

// ---------- 单聊 membership 自愈 ----------

// personRepairAttemptTTL 同一 (uid, 对端) 的修复尝试间隔：修复是幂等的，但坏会话
// 被反复打开时不必每次都打修复接口。
const personRepairAttemptTTL = 5 * time.Minute

var personRepairAttempts sync.Map // "uid|peer" -> time.Time

// repairPersonMembershipV3 尝试补建单聊缺失的 membership 行（v3 引擎的
// /conversations/membership/repair，create-if-absent，绝不动既有行）。
//
// 背景：目录投影丢一侧后，activate 修不了（raft 异步谎报 200）、发消息也补不了
// （投影状态门），用户打开单聊永远是空白。
// 返回 true 表示修复请求已被引擎接受，调用方可稍候重试一次原请求。
func (c *Context) repairPersonMembershipV3(loginUID, channelID string) bool {
	loginUID = strings.TrimSpace(loginUID)
	channelID = strings.TrimSpace(channelID)
	if loginUID == "" || channelID == "" {
		return false
	}
	key := loginUID + "|" + channelID
	if v, ok := personRepairAttempts.Load(key); ok {
		if at, ok := v.(time.Time); ok && time.Since(at) < personRepairAttemptTTL {
			return false
		}
	}
	personRepairAttempts.Store(key, time.Now())
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversations/membership/repair", []byte(util.ToJson(map[string]interface{}{
		"uid":          loginUID,
		"channel_id":   channelID,
		"channel_type": common.ChannelTypePerson.Uint8(),
	})), nil)
	if err != nil {
		c.Warn("v3单聊membership修复请求失败", zap.String("uid", loginUID), zap.String("channel_id", channelID), zap.Error(err))
		return false
	}
	if err = c.handlerIMError(resp); err != nil {
		// 旧引擎没有该路由（404）时静默退化为原行为
		c.Warn("v3单聊membership修复被拒", zap.String("uid", loginUID), zap.String("channel_id", channelID), zap.Error(err))
		return false
	}
	c.Info("v3单聊membership已提交修复", zap.String("uid", loginUID), zap.String("channel_id", channelID))
	return true
}

// emptySyncChannelMessageRespV3 构造与请求同形的空结果，让调用方走「没有更多消息」分支
func emptySyncChannelMessageRespV3(req SyncChannelMessageReq) *SyncChannelMessageResp {
	return &SyncChannelMessageResp{
		StartMessageSeq: req.StartMessageSeq,
		EndMessageSeq:   req.EndMessageSeq,
		PullMode:        req.PullMode,
		Messages:        make([]*MessageResp, 0),
	}
}

// groupCMDSubscriberBatchSize 群 CMD 定向投递的分批大小。
// v3 的 request-scoped 频道 id 是订阅者列表的哈希，一次带太多 uid 会让请求体过大，
// 分批发既能控制单次体积，也让部分失败不至于整群收不到。
const groupCMDSubscriberBatchSize = 500

// groupConversationActivateWorkers 限制一次批量拉人时对 v3 会话激活接口的并发。
// 激活是幂等的；部分成功后重试整批不会制造重复会话。
const groupConversationActivateWorkers = 8

// groupConversationRepairMaxPerSync 单次会话同步最多修复的缺失群会话数（其余留给后续 sync 分批补齐）。
const groupConversationRepairMaxPerSync = 32

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

	workerCount := min(groupConversationActivateWorkers, len(uids))
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
		batches = append(batches, uids[start:min(start+size, len(uids))])
	}
	return batches
}
