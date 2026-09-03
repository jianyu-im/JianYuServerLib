package config

// WuKongIM v3 原生接口层（/v3 轨专用）。
//
// 与同包的 msg_v3.go 是两种东西，别混：
//
//   - msg_v3.go 是**翻译层**。它把 v3 的目录式会话（游标分页 + active_at + last_message）
//     折回 v2 的快照式会话（version + recents + timestamp），好让存量客户端一行不改地继续跑。
//     8-24 以来的会话事故几乎都出在那层折叠上——它在对抗架构，所以只进入「只修 P0」的维护态。
//   - 本文件是**透传层**。它只做三件事：发 HTTP、解 JSON、把量纲归一到毫秒。
//     不合成 version、不折 recents、不猜时间戳、不做 fail-closed 判断。
//
// 硬性约束（对应规划 §4.2，每一条都是一次线上事故换来的）：
//
//  1. 时间一律毫秒，字段名带 _ms 后缀，纳秒禁止裸露（active_at 纳秒当毫秒用过一次，
//     会话被 16 位时间戳永久钉在列表顶部）。
//  2. seq 一律 uint64，且响应同时给字符串形式（JSON number 超过 2^53 客户端会静默丢精度）。
//  3. 单条会话解析失败进 unresolved，绝不让整页失败（fail-closed 断连事故的直接教训）。
//     所以本层遇到解析不了的行不返回 error，只把 key 记进 Unresolved。
//  4. next_cursor 是不透明字符串，只搬运不解析。
//
// 本文件不得调用 msg_v3.go 里的折叠函数（imSyncUserConversationV3 / conversationVersionV3 /
// conversationTimestampV3 / backfill* / reconcile* 等）。共享的只有纯函数：量纲归一与占位消息判定，
// 且实现的**唯一副本**在本文件，msg_v3.go 反过来委托给这里。

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jianyu-im/JianYuServerLib/pkg/network"
	"github.com/jianyu-im/JianYuServerLib/pkg/util"
)

// ---------- 量纲与占位消息（本包唯一实现，msg_v3.go 委托到这里） ----------

// V3NormalizeEpochMS 把量纲不明的 epoch 时间归一化到毫秒。
//
// v3 引擎里同一个「时间」有三种量纲同时存在：membership.activated_at 是
// time.Now().UnixNano()（纳秒），迁移导入的行是 0，消息时间戳是秒。按数量级判定：
// 秒 ~2e9、毫秒 ~2e12、微秒 ~2e15、纳秒 ~2e18，各量纲间隔千倍，边界取 1e11/1e14/1e17
// 在 5138 年前都不会误判。
func V3NormalizeEpochMS(v int64) int64 {
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

// V3ConversationActiveAtMS 解析一条会话的排序权威时间（毫秒）。
//
// 规划 §4.3① 把排序权威写成 active_at_ms，前提是「引擎每条消息写入时更新 active_at」——
// **实测这个前提不成立**：引擎里 activated_at 的唯一写入方是 POST /conversations/activate
// （internal/usecase/conversation/unread.go 的注释明说 send/receive/deliver/pull 都不写它），
// 发消息不会 bump 它。线上绝大多数会话 active_at=0，只有被会话对账 activate 过的才有值。
// 引擎按用户要求不改，所以真正的排序权威只能在本层合成：
//
//	active_at_ms = max(归一化(引擎 active_at), 最后一条消息时间)
//
// 消息时间打底保证「刚说过话的会话在最前」，active_at 兜住骨架会话（无可见消息但用户开过）。
// 两者都为 0 时返回 0，客户端把它排在最后即可，不要当成 1970 年展示。
func V3ConversationActiveAtMS(activeAt int64, lastMessageMS int64) int64 {
	ms := V3NormalizeEpochMS(activeAt)
	if lm := V3NormalizeEpochMS(lastMessageMS); lm > ms {
		ms = lm
	}
	if ms <= 0 {
		return 0
	}
	return ms
}

// IsV3SeqPadClientMsgNo 判断是不是 2026-08-23 迁移补 seq 灌进去的占位消息。
// 占位消息只为把 v3 的 seq 水位追上生产库，对用户不可见，绝不能当会话末条。
func IsV3SeqPadClientMsgNo(clientMsgNo string) bool {
	return strings.HasPrefix(clientMsgNo, seqPadClientMsgNoPrefix)
}

// ---------- 错误 ----------

var (
	// ErrV3EngineDisabled 当前配置不是 v3 引擎，/v3 轨不可用。
	ErrV3EngineDisabled = errors.New("v3原生接口仅在 im.engine=v3 时可用")
	// ErrV3CursorStalled 游标不前进且 done=false，属于服务端 bug，调用方应带 cursor 值上报。
	ErrV3CursorStalled = errors.New("v3会话目录游标未前进")
)

// V3EngineEnabled 是否可以走 /v3 轨。
func (c *Context) V3EngineEnabled() bool {
	return c.imEngineV3()
}

// IsV3MembershipRequiredErr 判断错误是不是 v3 的「不是该频道成员」。
// 单聊撞到它多半是目录投影丢了一侧 membership，可以自愈；群撞到它是真的不在群里。
func IsV3MembershipRequiredErr(err error) bool { return isMembershipRequiredErrV3(err) }

// IsV3ChannelAbsentErr 判断错误是不是 v3 的「频道不存在」。
func IsV3ChannelAbsentErr(err error) bool { return isChannelAbsentErrV3(err) }

// ---------- 会话目录 ----------

// V3ChannelKey 频道标识。channel_id 已由引擎按 login_uid 还原成对端 uid（单聊），
// 不会漏出 a@b 形式——客户端主键零迁移是硬性契约。
type V3ChannelKey struct {
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
}

// V3LastMessage 会话末条消息。
type V3LastMessage struct {
	MessageID     uint64 `json:"-"`
	MessageIDStr  string `json:"message_id_str"`
	MessageSeq    uint64 `json:"-"`
	MessageSeqStr string `json:"message_seq_str"`
	FromUID       string `json:"from_uid"`
	ClientMsgNo   string `json:"client_msg_no"`
	TimestampMS   int64  `json:"timestamp_ms"`
	// Payload 是**已解码的原始字节**，不是 json.RawMessage。
	//
	// 引擎那边这个字段是 Go 的 []byte，序列化成 JSON 就是一个 **base64 字符串**。
	// 声明成 json.RawMessage 会拿到带引号的 `"eyJjb250..."`，调用方再去 Unmarshal
	// 成对象必然失败、落到「消息解析异常」占位——而且**每一条消息都会这样**，
	// 症状是整个会话列表的预览全变成错误占位，服务端却一切正常。
	// 声明成 []byte 则由 encoding/json 自动完成 base64 解码，拿到的就是真正的 JSON 正文。
	Payload []byte `json:"payload"`
}

// V3Conversation 一条目录式会话。业务字段（置顶/免打扰/备注等）由 jyserver 合并，不在本层。
type V3Conversation struct {
	ChannelID    string
	ChannelType  uint8
	ActiveAtMS   int64
	ReadSeq      uint64
	DeletedToSeq uint64
	Unread       uint64
	LastMessage  *V3LastMessage
}

// V3ConversationPage 一页会话目录，字段与引擎 /conversation/list 一一对应。
type V3ConversationPage struct {
	Conversations []*V3Conversation
	Deletes       []V3ChannelKey
	Unresolved    []V3ChannelKey
	NextCursor    string
	Done          bool
	Coverage      int64
	ResetRequired bool
}

// v3RawLastMessage 引擎 /conversation/list 的 last_message 原始形状。
type v3RawLastMessage struct {
	MessageID         uint64 `json:"message_id"`
	MessageIDStr      string `json:"message_idstr"`
	MessageSeq        uint64 `json:"message_seq"`
	FromUID           string `json:"from_uid"`
	ClientMsgNo       string `json:"client_msg_no"`
	ServerTimestampMS int64  `json:"server_timestamp_ms"`
	// 引擎侧是 []byte → JSON 里是 base64 字符串，必须用 []byte 接才会被自动解码
	Payload []byte `json:"payload"`
}

type v3RawConversation struct {
	ChannelID    string            `json:"channel_id"`
	ChannelType  int64             `json:"channel_type"`
	ActiveAt     int64             `json:"active_at"`
	ReadSeq      uint64            `json:"read_seq"`
	DeletedToSeq uint64            `json:"deleted_to_seq"`
	Unread       uint64            `json:"unread"`
	LastMessage  *v3RawLastMessage `json:"last_message"`
}

type v3RawChannelKey struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int64  `json:"channel_type"`
}

type v3RawConversationPage struct {
	Conversations []v3RawConversation `json:"conversations"`
	Deletes       []v3RawChannelKey   `json:"deletes"`
	Unresolved    []v3RawChannelKey   `json:"unresolved"`
	NextCursor    string              `json:"next_cursor"`
	Done          bool                `json:"done"`
	Coverage      int64               `json:"coverage"`
	ResetRequired bool                `json:"reset_required"`
}

// V3ConversationListReq 一次目录分页请求。
type V3ConversationListReq struct {
	UID string
	// Cursor 上一页的 next_cursor，空串表示从头开始。不透明，本层只搬运。
	Cursor string
	Limit  int
	// CompletedCoverage 客户端上一次「完整走完一轮」的 coverage。引擎用它判断客户端
	// 的游标是否早于服务端还保留的墓碑，早了就回 reset_required。
	CompletedCoverage int64
}

// V3ConversationList 拉一页会话目录。**只发一次请求、只解一页**，不在本层做全量循环：
// 全量聚合是翻译层为了拼 v2 快照才做的事，正是它把一个坏会话放大成整张列表报废。
func (c *Context) V3ConversationList(req V3ConversationListReq) (*V3ConversationPage, error) {
	if !c.imEngineV3() {
		return nil, ErrV3EngineDisabled
	}
	if strings.TrimSpace(req.UID) == "" {
		return nil, errors.New("uid不能为空")
	}
	body := map[string]interface{}{"uid": req.UID}
	if req.Limit > 0 {
		body["limit"] = req.Limit
	}
	if req.Cursor != "" {
		body["cursor"] = req.Cursor
	}
	if req.CompletedCoverage > 0 {
		body["completed_coverage"] = req.CompletedCoverage
	}
	return c.v3PostConversationPage("/conversation/list", body)
}

// V3ConversationRetry 补拉一批 unresolved 会话。引擎对这批不推进 coverage，可安全重试。
func (c *Context) V3ConversationRetry(uid string, keys []V3ChannelKey) (*V3ConversationPage, error) {
	if !c.imEngineV3() {
		return nil, ErrV3EngineDisabled
	}
	if strings.TrimSpace(uid) == "" {
		return nil, errors.New("uid不能为空")
	}
	if len(keys) == 0 {
		return &V3ConversationPage{
			Conversations: []*V3Conversation{},
			Deletes:       []V3ChannelKey{},
			Unresolved:    []V3ChannelKey{},
			Done:          true,
		}, nil
	}
	rawKeys := make([]v3RawChannelKey, 0, len(keys))
	for _, key := range keys {
		if key.ChannelID == "" || key.ChannelType == 0 {
			continue
		}
		rawKeys = append(rawKeys, v3RawChannelKey{ChannelID: key.ChannelID, ChannelType: int64(key.ChannelType)})
	}
	return c.v3PostConversationPage("/conversation/retry", map[string]interface{}{
		"uid": uid, "channels": rawKeys,
	})
}

func (c *Context) v3PostConversationPage(path string, body map[string]interface{}) (*V3ConversationPage, error) {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+path, []byte(util.ToJson(body)), nil)
	if err != nil {
		return nil, err
	}
	if err := c.handlerIMError(resp); err != nil {
		return nil, err
	}
	var raw v3RawConversationPage
	if err := util.ReadJsonByByte([]byte(resp.Body), &raw); err != nil {
		return nil, err
	}
	return newV3ConversationPage(raw), nil
}

func newV3ConversationPage(raw v3RawConversationPage) *V3ConversationPage {
	page := &V3ConversationPage{
		Conversations: make([]*V3Conversation, 0, len(raw.Conversations)),
		Deletes:       make([]V3ChannelKey, 0, len(raw.Deletes)),
		Unresolved:    make([]V3ChannelKey, 0, len(raw.Unresolved)),
		NextCursor:    raw.NextCursor,
		Done:          raw.Done,
		Coverage:      raw.Coverage,
		ResetRequired: raw.ResetRequired,
	}
	for _, item := range raw.Conversations {
		conv := newV3Conversation(item)
		if conv == nil {
			// 频道类型越界等畸形行：进 unresolved 而不是丢弃，也不是整页报错。
			// 客户端会对 unresolved 调 retry，仍失败就灰条降级，永远不会静默少一条会话。
			page.Unresolved = append(page.Unresolved, V3ChannelKey{
				ChannelID: item.ChannelID, ChannelType: v3ClampChannelType(item.ChannelType),
			})
			continue
		}
		page.Conversations = append(page.Conversations, conv)
	}
	for _, key := range raw.Deletes {
		page.Deletes = append(page.Deletes, V3ChannelKey{ChannelID: key.ChannelID, ChannelType: v3ClampChannelType(key.ChannelType)})
	}
	for _, key := range raw.Unresolved {
		page.Unresolved = append(page.Unresolved, V3ChannelKey{ChannelID: key.ChannelID, ChannelType: v3ClampChannelType(key.ChannelType)})
	}
	return page
}

func v3ClampChannelType(channelType int64) uint8 {
	if channelType <= 0 || channelType > 255 {
		return 0
	}
	return uint8(channelType)
}

func newV3Conversation(raw v3RawConversation) *V3Conversation {
	channelType := v3ClampChannelType(raw.ChannelType)
	if raw.ChannelID == "" || channelType == 0 {
		return nil
	}
	var last *V3LastMessage
	var lastMS int64
	if raw.LastMessage != nil {
		lastMS = V3NormalizeEpochMS(raw.LastMessage.ServerTimestampMS)
		idStr := raw.LastMessage.MessageIDStr
		if idStr == "" {
			idStr = strconv.FormatUint(raw.LastMessage.MessageID, 10)
		}
		last = &V3LastMessage{
			MessageID:     raw.LastMessage.MessageID,
			MessageIDStr:  idStr,
			MessageSeq:    raw.LastMessage.MessageSeq,
			MessageSeqStr: strconv.FormatUint(raw.LastMessage.MessageSeq, 10),
			FromUID:       raw.LastMessage.FromUID,
			ClientMsgNo:   raw.LastMessage.ClientMsgNo,
			TimestampMS:   lastMS,
			Payload:       raw.LastMessage.Payload,
		}
	}
	return &V3Conversation{
		ChannelID:    raw.ChannelID,
		ChannelType:  channelType,
		ActiveAtMS:   V3ConversationActiveAtMS(raw.ActiveAt, lastMS),
		ReadSeq:      raw.ReadSeq,
		DeletedToSeq: raw.DeletedToSeq,
		Unread:       raw.Unread,
		LastMessage:  last,
	}
}

// ---------- 会话状态写入 ----------

// V3ClearConversationUnread 清一个会话的未读（引擎把 read_seq 推到频道尾）。
func (c *Context) V3ClearConversationUnread(uid, channelID string, channelType uint8) error {
	return c.v3PostConversationMutation("/conversations/clearUnread", map[string]interface{}{
		"uid": uid, "channel_id": channelID, "channel_type": channelType,
	})
}

// V3SetConversationUnread 把未读设成指定条数（引擎反推 read_seq）。
func (c *Context) V3SetConversationUnread(uid, channelID string, channelType uint8, unread int) error {
	if unread < 0 {
		unread = 0
	}
	return c.v3PostConversationMutation("/conversations/setUnread", map[string]interface{}{
		"uid": uid, "channel_id": channelID, "channel_type": channelType, "unread": unread,
	})
}

// V3DeleteConversation 删除会话（引擎写墓碑，会话从目录消失）。
// 注意这不是清屏：清屏要保留会话本身，引擎没有对应路由，见 V3ClearChannelHistoryFloor 的说明。
func (c *Context) V3DeleteConversation(uid, channelID string, channelType uint8) error {
	return c.v3PostConversationMutation("/conversations/delete", map[string]interface{}{
		"uid": uid, "channel_id": channelID, "channel_type": channelType,
	})
}

// V3ActivateConversation 把会话提到目录顶部（引擎写 activated_at=now，纳秒）。
func (c *Context) V3ActivateConversation(uid, channelID string, channelType uint8) error {
	return c.v3PostConversationMutation("/conversations/activate", map[string]interface{}{
		"uid": uid, "channel_id": channelID, "channel_type": channelType,
	})
}

// V3RepairPersonMembership 补建单聊缺失的 UID-owned membership（create-if-absent，仅 channel_type=1）。
// 已存在的行（含墓碑）原样不动，幂等可重入。
func (c *Context) V3RepairPersonMembership(uid, channelID string) error {
	return c.v3PostConversationMutation("/conversations/membership/repair", map[string]interface{}{
		"uid": uid, "channel_id": channelID, "channel_type": 1,
	})
}

func (c *Context) v3PostConversationMutation(path string, body map[string]interface{}) error {
	if !c.imEngineV3() {
		return ErrV3EngineDisabled
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+path, []byte(util.ToJson(body)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// ---------- 频道消息同步 ----------

// V3MessageHeader 消息头。
type V3MessageHeader struct {
	NoPersist int `json:"no_persist"`
	RedDot    int `json:"red_dot"`
	SyncOnce  int `json:"sync_once"`
}

// V3Message 一条消息。seq/id 一律 uint64 且带字符串形式；event_meta / stream_data 原样透传。
type V3Message struct {
	Header        V3MessageHeader `json:"header"`
	Setting       uint8           `json:"setting"`
	MessageID     uint64          `json:"-"`
	MessageIDStr  string          `json:"message_id_str"`
	MessageSeq    uint64          `json:"-"`
	MessageSeqStr string          `json:"message_seq_str"`
	ClientMsgNo   string          `json:"client_msg_no"`
	StreamNo      string          `json:"stream_no,omitempty"`
	FromUID       string          `json:"from_uid"`
	ChannelID     string          `json:"channel_id"`
	ChannelType   uint8           `json:"channel_type"`
	Topic         string          `json:"topic,omitempty"`
	Expire        uint32          `json:"expire,omitempty"`
	TimestampMS   int64           `json:"timestamp_ms"`
	// 同 V3LastMessage.Payload：引擎侧是 []byte，JSON 里是 base64 字符串，
	// 必须用 []byte 接才会被自动解码成真正的正文。
	Payload []byte `json:"payload"`

	// 以下三个是 v3 新增的流/事件语义，本层不解释只搬运。
	// 不认识它们的端按未知字段忽略即可，绝不能因此断连。
	// StreamData 引擎侧同样是 []byte（base64）；EventMeta/EventSyncHint 是结构体，
	// JSON 里是真正的对象，所以那两个用 RawMessage 透传才对。**三个字段不能一视同仁。**
	StreamData    []byte          `json:"stream_data,omitempty"`
	EventMeta     json.RawMessage `json:"event_meta,omitempty"`
	EventSyncHint json.RawMessage `json:"event_sync_hint,omitempty"`
	End           uint8           `json:"end,omitempty"`
	EndReason     uint8           `json:"end_reason,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// V3ChannelSyncReq 一次频道消息同步。seq 语义与引擎一致：start 含界、end 排他，
// start=0 且 end=0 表示「从最新往回读一屏」。
type V3ChannelSyncReq struct {
	LoginUID        string
	ChannelID       string
	ChannelType     uint8
	StartMessageSeq uint64
	EndMessageSeq   uint64
	Limit           int
	PullMode        PullMode
	// IncludeEventMeta 透传给引擎，客户端要什么就给什么，本层不代客户端做决定。
	IncludeEventMeta bool
	EventSummaryMode string
}

// V3ChannelSyncResp 一次频道消息同步的结果。
type V3ChannelSyncResp struct {
	ChannelID       string
	ChannelType     uint8
	StartMessageSeq uint64
	EndMessageSeq   uint64
	More            int
	Messages        []*V3Message
	// Err 仅 batch 使用：单个频道失败不拖垮整批。
	Err error
}

type v3RawMessage struct {
	Header        V3MessageHeader `json:"header"`
	Setting       uint8           `json:"setting"`
	MessageID     int64           `json:"message_id"`
	MessageIDStr  string          `json:"message_idstr"`
	ClientMsgNo   string          `json:"client_msg_no"`
	MessageSeq    uint64          `json:"message_seq"`
	FromUID       string          `json:"from_uid"`
	ChannelID     string          `json:"channel_id"`
	ChannelType   uint8           `json:"channel_type"`
	Topic         string          `json:"topic"`
	Expire        uint32          `json:"expire"`
	Timestamp     int64           `json:"timestamp"`
	Payload       []byte          `json:"payload"`
	StreamNo      string          `json:"stream_no"`
	StreamData    []byte          `json:"stream_data"`
	EventMeta     json.RawMessage `json:"event_meta"`
	EventSyncHint json.RawMessage `json:"event_sync_hint"`
	End           uint8           `json:"end"`
	EndReason     uint8           `json:"end_reason"`
	Error         string          `json:"error"`
}

type v3RawChannelSyncResp struct {
	StartMessageSeq uint64         `json:"start_message_seq"`
	EndMessageSeq   uint64         `json:"end_message_seq"`
	More            int            `json:"more"`
	Messages        []v3RawMessage `json:"messages"`
}

type v3RawChannelSyncBatchItem struct {
	ChannelID       string         `json:"channel_id"`
	ChannelType     uint8          `json:"channel_type"`
	StartMessageSeq uint64         `json:"start_message_seq"`
	EndMessageSeq   uint64         `json:"end_message_seq"`
	More            int            `json:"more"`
	Messages        []v3RawMessage `json:"messages"`
	Error           string         `json:"error"`
}

type v3RawChannelSyncBatchResp struct {
	Items []v3RawChannelSyncBatchItem `json:"items"`
}

func (r V3ChannelSyncReq) toBody() map[string]interface{} {
	body := map[string]interface{}{
		"login_uid":         r.LoginUID,
		"channel_id":        r.ChannelID,
		"channel_type":      r.ChannelType,
		"start_message_seq": r.StartMessageSeq,
		"end_message_seq":   r.EndMessageSeq,
		"limit":             r.Limit,
		"pull_mode":         r.PullMode,
	}
	if r.IncludeEventMeta {
		body["include_event_meta"] = 1
	}
	if r.EventSummaryMode != "" {
		body["event_summary_mode"] = r.EventSummaryMode
	}
	return body
}

// V3ChannelMessageSync 同步一个频道的消息。
func (c *Context) V3ChannelMessageSync(req V3ChannelSyncReq) (*V3ChannelSyncResp, error) {
	if !c.imEngineV3() {
		return nil, ErrV3EngineDisabled
	}
	if strings.TrimSpace(req.LoginUID) == "" {
		// v3 用 login_uid 归一化单聊频道，空值会拼出一个不存在的频道并静默返回空。
		return nil, errors.New("login_uid不能为空")
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesync", []byte(util.ToJson(req.toBody())), nil)
	if err != nil {
		return nil, err
	}
	if err := c.handlerIMError(resp); err != nil {
		return nil, err
	}
	var raw v3RawChannelSyncResp
	if err := util.ReadJsonByByte([]byte(resp.Body), &raw); err != nil {
		return nil, err
	}
	out := &V3ChannelSyncResp{
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		// 引擎响应里的 start/end 是回声，pull_mode 干脆不回；按请求回填才是调用方要的语义。
		StartMessageSeq: req.StartMessageSeq,
		EndMessageSeq:   req.EndMessageSeq,
		More:            raw.More,
		Messages:        newV3Messages(raw.Messages),
	}
	return out, nil
}

// V3ChannelMessageSyncBatch 一次同步多个频道（冷启动预热用，省掉首屏 N 次往返）。
// 单个频道失败只写进对应条目的 Err，不影响其余频道——批量接口最容易犯的错就是
// 一个坏频道让整批 5xx，那等于把首屏全押在最脆弱的一个会话上。
func (c *Context) V3ChannelMessageSyncBatch(loginUID string, items []V3ChannelSyncReq) ([]*V3ChannelSyncResp, error) {
	if !c.imEngineV3() {
		return nil, ErrV3EngineDisabled
	}
	if strings.TrimSpace(loginUID) == "" {
		return nil, errors.New("login_uid不能为空")
	}
	if len(items) == 0 {
		return []*V3ChannelSyncResp{}, nil
	}
	bodyItems := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		item.LoginUID = loginUID
		bodyItems = append(bodyItems, item.toBody())
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesyncbatch", []byte(util.ToJson(map[string]interface{}{
		"login_uid": loginUID, "items": bodyItems,
	})), nil)
	if err != nil {
		return nil, err
	}
	if err := c.handlerIMError(resp); err != nil {
		return nil, err
	}
	var raw v3RawChannelSyncBatchResp
	if err := util.ReadJsonByByte([]byte(resp.Body), &raw); err != nil {
		return nil, err
	}
	results := make([]*V3ChannelSyncResp, 0, len(items))
	for index, item := range items {
		out := &V3ChannelSyncResp{
			ChannelID:       item.ChannelID,
			ChannelType:     item.ChannelType,
			StartMessageSeq: item.StartMessageSeq,
			EndMessageSeq:   item.EndMessageSeq,
			Messages:        []*V3Message{},
		}
		if index < len(raw.Items) {
			rawItem := raw.Items[index]
			out.More = rawItem.More
			out.Messages = newV3Messages(rawItem.Messages)
			if rawItem.Error != "" {
				out.Err = errors.New(rawItem.Error)
			}
		} else {
			out.Err = fmt.Errorf("v3 messagesyncbatch 少返回了第 %d 个频道", index)
		}
		results = append(results, out)
	}
	return results, nil
}

// V3ChannelMaxSeq 查频道当前真实最大 seq。
//
// v3 没有 GET /channel/max_message_seq，用 messagesync 从最新往回读一条推导。
// 这个值是毒化水位守卫的判据：任何隐藏水位一旦 ≥ 它，说明水位是脏的（重建压缩过 seq、
// 或事件重试泵写过越界值），必须按 0 处理而不是让用户看见一个空频道。
func (c *Context) V3ChannelMaxSeq(loginUID, channelID string, channelType uint8) (uint64, error) {
	resp, err := c.V3ChannelMessageSync(V3ChannelSyncReq{
		LoginUID:    loginUID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Limit:       1,
		PullMode:    PullModeDown,
	})
	if err != nil {
		return 0, err
	}
	var maxSeq uint64
	for _, msg := range resp.Messages {
		if msg != nil && msg.MessageSeq > maxSeq {
			maxSeq = msg.MessageSeq
		}
	}
	return maxSeq, nil
}

func newV3Messages(raw []v3RawMessage) []*V3Message {
	out := make([]*V3Message, 0, len(raw))
	for _, item := range raw {
		if msg := newV3Message(item); msg != nil {
			out = append(out, msg)
		}
	}
	return out
}

func newV3Message(raw v3RawMessage) *V3Message {
	idStr := raw.MessageIDStr
	if idStr == "" {
		idStr = strconv.FormatInt(raw.MessageID, 10)
	}
	messageID := uint64(0)
	if raw.MessageID > 0 {
		messageID = uint64(raw.MessageID)
	} else if parsed, err := strconv.ParseUint(idStr, 10, 64); err == nil {
		// 引擎的 message_id 走的是 int64 字段，超过 2^63 会翻负；字符串形式才是权威。
		messageID = parsed
	}
	return &V3Message{
		Header:        raw.Header,
		Setting:       raw.Setting,
		MessageID:     messageID,
		MessageIDStr:  idStr,
		MessageSeq:    raw.MessageSeq,
		MessageSeqStr: strconv.FormatUint(raw.MessageSeq, 10),
		ClientMsgNo:   raw.ClientMsgNo,
		StreamNo:      raw.StreamNo,
		FromUID:       raw.FromUID,
		ChannelID:     raw.ChannelID,
		ChannelType:   raw.ChannelType,
		Topic:         raw.Topic,
		Expire:        raw.Expire,
		// 引擎的 timestamp 是秒；出口一律毫秒，这是 /v3 轨唯一做量纲转换的地方。
		TimestampMS:   V3NormalizeEpochMS(raw.Timestamp),
		Payload:       raw.Payload,
		StreamData:    raw.StreamData,
		EventMeta:     raw.EventMeta,
		EventSyncHint: raw.EventSyncHint,
		End:           raw.End,
		EndReason:     raw.EndReason,
		Error:         raw.Error,
	}
}

// ---------- 隐藏水位 ----------

// V3VisibilityFloor 解析一个会话的隐藏水位（低于等于它的消息不该展示给该用户）。
//
// 规划 §4.3② 原本写「MySQL channel_offset 在 /v3 轨一律不参与过滤」，前提是清屏动作
// 在引擎里有落点。实测引擎只有 /conversations/delete（整会话墓碑），**没有**「清到某个 seq」
// 的写接口；按用户要求引擎不改，所以清屏水位只能继续留在 MySQL。
//
// 于是契约改成：两个水位取大，但**都过毒化守卫**——
//
//	水位 > 频道真实 max_seq  ==>  按 0 处理
//
// 两次「整群一条消息都看不到」的线上事故（重建压缩 seq 后 MySQL offset 没降、
// memberadd 重试泵每 2 分钟重写越界 offset）都是水位**严格大于**频道真实 max，守卫能兜住；
// 同时清屏功能保持可用。
//
// 判据必须是严格大于，不能是大于等于：`水位 == 频道 max` 是完全正常的状态——
// 用户刚清完屏、或刚以隐藏历史身份入群，此后频道没有新消息，水位就正好等于 max。
// 用 >= 会把这种会话的水位抹成 0，把用户清掉的记录、以及隐藏历史群里入群前的消息
// 全部翻出来。那比「看不到」严重得多：一个是少看见，一个是不该看的看见了。
//
// channelMaxSeq 传 0 表示未知，此时不做守卫只取大值。
func V3VisibilityFloor(engineDeletedToSeq, mysqlChannelOffset, channelMaxSeq uint64) uint64 {
	floor := engineDeletedToSeq
	if mysqlChannelOffset > floor {
		floor = mysqlChannelOffset
	}
	if V3FloorPoisoned(floor, channelMaxSeq) {
		return 0
	}
	return floor
}

// V3FloorPoisoned 判断某个水位是不是毒化水位，用于打点告警（自愈但要留痕，
// 否则又会像 2026-08-24 那样，几百人整群看不到消息而服务端零报错）。
func V3FloorPoisoned(floor, channelMaxSeq uint64) bool {
	return channelMaxSeq > 0 && floor > channelMaxSeq
}

// ---------- 能力协商 ----------

// V3Capabilities 服务端下发给客户端的长连接权威参数（规划 §5.1）。
// 各端一律照抄，禁止自定——表里每一行都对应一次线上事故。
type V3Capabilities struct {
	// ProtocolVersionMin/Max wkproto 协商区间。v3 网关同端口兼容 v5/v6。
	ProtocolVersionMin int `json:"protocol_version_min"`
	ProtocolVersionMax int `json:"protocol_version_max"`
	// HeartbeatIntervalSeconds 客户端 ping 周期。
	HeartbeatIntervalSeconds int `json:"heartbeat_interval_seconds"`
	// HeartbeatTimeoutSeconds 网关侧 idle 超时。
	HeartbeatTimeoutSeconds int `json:"heartbeat_timeout_seconds"`
	// HeartbeatMissedLimit 连续几个周期收不到 pong 判死重连。
	HeartbeatMissedLimit int `json:"heartbeat_missed_limit"`
	// SendAckTimeoutMS 发送回执等待上限。winds 曾用 90s，超时窗口比服务端权威值大一个数量级，
	// 用户早就重发了才判失败。
	SendAckTimeoutMS int `json:"send_ack_timeout_ms"`
	// RecvAckRequired 收到 RECV 必回 RECVACK，**解码失败也要回**。
	// 拒收不回执会让服务端无限重传，放大器在回执缺失那层。
	RecvAckRequired bool `json:"recv_ack_required"`
	// DropUndecodableFrame 单帧解码失败丢该帧继续，不断连。
	DropUndecodableFrame bool `json:"drop_undecodable_frame"`
	// FailedSendAckMustShowError 非成功码必须画红叹号，禁止默认绿勾。
	FailedSendAckMustShowError bool `json:"failed_send_ack_must_show_error"`
	// DisconnectTriggersLogout v3 服务端从不主动发 DISCONNECT；收到就按异常重连处理，
	// **禁止触发退登**（iOS 红线）。踢号唯一信号是 CONNACK 拒绝码。
	DisconnectTriggersLogout bool `json:"disconnect_triggers_logout"`
	// KickReasonCodeAmbiguous kicked 与 ReasonAuthFail 撞值=2，需按端上下文区分。
	KickReasonCodeAmbiguous bool `json:"kick_reason_code_ambiguous"`
	// DeviceIDStable device_id 每设备持久化生成一次，重连不变（随机会造幽灵在线）。
	DeviceIDStable bool `json:"device_id_stable"`
	// EventFrameNo EVENT 帧编号；不支持的端按未知帧丢弃，不断连。
	EventFrameNo int `json:"event_frame_no"`
	// ConversationSyncPageLimit 建议的目录分页大小。
	ConversationSyncPageLimit int `json:"conversation_sync_page_limit"`
	// SeqFieldsAreStrings 提醒客户端一律读 *_str 字段（u64 超 2^53 JSON 会丢精度）。
	SeqFieldsAreStrings bool `json:"seq_fields_are_strings"`
	// TimeFieldsAreMilliseconds 提醒客户端所有 _ms 后缀字段都是毫秒。
	TimeFieldsAreMilliseconds bool `json:"time_fields_are_milliseconds"`
	// ServerTimeMS 服务端当前时间，客户端可据此校正本地时钟偏移。
	ServerTimeMS int64 `json:"server_time_ms"`
}

// V3DefaultCapabilities 返回规划 §5.1 权威参数表。
// 客户端启动时拉一次，拉不到用各端内置的同一份默认值。
func V3DefaultCapabilities() V3Capabilities {
	return V3Capabilities{
		ProtocolVersionMin:         5,
		ProtocolVersionMax:         6,
		HeartbeatIntervalSeconds:   30,
		HeartbeatTimeoutSeconds:    180,
		HeartbeatMissedLimit:       2,
		SendAckTimeoutMS:           5000,
		RecvAckRequired:            true,
		DropUndecodableFrame:       true,
		FailedSendAckMustShowError: true,
		DisconnectTriggersLogout:   false,
		KickReasonCodeAmbiguous:    true,
		DeviceIDStable:             true,
		EventFrameNo:               12,
		ConversationSyncPageLimit:  200,
		SeqFieldsAreStrings:        true,
		TimeFieldsAreMilliseconds:  true,
		ServerTimeMS:               time.Now().UnixMilli(),
	}
}
