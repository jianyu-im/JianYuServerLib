package config

// 上游 WuKongIM v3（main 分支）适配层。
//
// 多产品共用本库：v2 产品（jianyuim fork）行为完全不变；
// 仅当配置 im.engine=v3 时，msg.go 中对应函数分流到本文件的等价实现。
// v3 与 v2 的接口差异见 customer/docs/wukongim-v3-standalone-deploy.md §6。

import (
	"fmt"
	"sort"
	"strings"

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

// conversationListCursor v3 /conversation/list 游标
type conversationListCursor struct {
	ActiveAt    int64  `json:"active_at"`
	ChannelID   string `json:"channel_id"`
	ChannelType int64  `json:"channel_type"`
}

// conversationListResp v3 /conversation/list 响应
type conversationListResp struct {
	Conversations []struct {
		ChannelID   string `json:"channel_id"`
		ChannelType int64  `json:"channel_type"`
		ActiveAt    int64  `json:"active_at"` // 毫秒（实测 v3 返回 13 位时间戳）
		Unread      uint64 `json:"unread"`
		LastMessage *struct {
			MessageID         uint64 `json:"message_id"`
			MessageIDStr      string `json:"message_idstr"`
			MessageSeq        uint64 `json:"message_seq"`
			FromUID           string `json:"from_uid"`
			ClientMsgNo       string `json:"client_msg_no"`
			ServerTimestampMS int64  `json:"server_timestamp_ms"`
			Payload           []byte `json:"payload"`
		} `json:"last_message"`
	} `json:"conversations"`
	NextCursor *conversationListCursor `json:"next_cursor"`
	More       int                     `json:"more"`
}

// imGetConversationsV3 v3 无 GET /conversations，用 POST /conversation/list（游标分页）等价聚合，
// 输出结构保持 v2 的 ConversationResp 不变
func (c *Context) imGetConversationsV3(uid string) ([]*ConversationResp, error) {
	results := make([]*ConversationResp, 0)
	var cursor *conversationListCursor
	// 分页上限兜底，防异常游标死循环（100页×200条足够覆盖单用户会话数）
	for page := 0; page < 100; page++ {
		reqMap := map[string]interface{}{"uid": uid, "limit": 200}
		if cursor != nil {
			reqMap["cursor"] = cursor
		}
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversation/list", []byte(util.ToJson(reqMap)), nil)
		if err != nil {
			return nil, err
		}
		if err := c.handlerIMError(resp); err != nil {
			return nil, err
		}
		var listResp conversationListResp
		if err := util.ReadJsonByByte([]byte(resp.Body), &listResp); err != nil {
			return nil, err
		}
		for _, conv := range listResp.Conversations {
			item := &ConversationResp{
				ChannelID:   conv.ChannelID,
				ChannelType: uint8(conv.ChannelType),
				Unread:      int64(conv.Unread),
				Timestamp:   conv.ActiveAt / 1000, // v3 active_at 为毫秒，v2 timestamp 为秒
			}
			if conv.LastMessage != nil {
				item.LastMessage = &MessageResp{
					MessageID:    int64(conv.LastMessage.MessageID),
					MessageIDStr: conv.LastMessage.MessageIDStr,
					MessageSeq:   uint32(conv.LastMessage.MessageSeq),
					ClientMsgNo:  conv.LastMessage.ClientMsgNo,
					FromUID:      conv.LastMessage.FromUID,
					ChannelID:    conv.ChannelID,
					ChannelType:  uint8(conv.ChannelType),
					Timestamp:    int32(conv.LastMessage.ServerTimestampMS / 1000),
					Payload:      conv.LastMessage.Payload,
				}
			}
			results = append(results, item)
		}
		if listResp.More != 1 || listResp.NextCursor == nil {
			break
		}
		cursor = listResp.NextCursor
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
//   - version 增量：v3 没有会话版本号，参数被忽略，每次返回全量会话；返回的 Version 恒为 0，
//     客户端拿不到可回传的增量水位，等价于每次全量刷新会话列表。
//   - Recents：v2 会带回每个会话最近 msgCount 条消息；v3 的 list 每个会话只给 last_message，
//     这里只填 1 条。逐会话再调 messagesync 补齐会把一次同步放大成 N 次 HTTP，得不偿失；
//     客户端进入会话后本来就会走 /message/channel/sync 拉历史。
//   - lastMsgSeqs / larges：v3 无对应入参（超大群不再需要调用方声明），忽略。
func (c *Context) imSyncUserConversationV3(uid string, msgCount int64) ([]*SyncUserConversationResp, error) {
	results := make([]*SyncUserConversationResp, 0)
	var cursor *conversationListCursor
	// 分页上限兜底，防异常游标死循环（100页×200条足够覆盖单用户会话数）
	for page := 0; page < 100; page++ {
		reqMap := map[string]interface{}{"uid": uid, "limit": 200}
		if cursor != nil {
			reqMap["cursor"] = cursor
		}
		resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversation/list", []byte(util.ToJson(reqMap)), nil)
		if err != nil {
			return nil, err
		}
		if err := c.handlerIMError(resp); err != nil {
			return nil, err
		}
		var listResp conversationListResp
		if err := util.ReadJsonByByte([]byte(resp.Body), &listResp); err != nil {
			return nil, err
		}
		for _, conv := range listResp.Conversations {
			item := &SyncUserConversationResp{
				ChannelID:   conv.ChannelID,
				ChannelType: uint8(conv.ChannelType),
				Unread:      int(conv.Unread),
				Timestamp:   conv.ActiveAt / 1000, // v3 active_at 为毫秒，v2 timestamp 为秒
				Version:     0,                    // v3 无会话版本号
				Recents:     make([]*MessageResp, 0, 1),
			}
			if conv.LastMessage != nil {
				item.LastMsgSeq = int64(conv.LastMessage.MessageSeq)
				item.LastClientMsgNo = conv.LastMessage.ClientMsgNo
				if msgCount != 0 {
					item.Recents = append(item.Recents, &MessageResp{
						MessageID:    int64(conv.LastMessage.MessageID),
						MessageIDStr: conv.LastMessage.MessageIDStr,
						MessageSeq:   uint32(conv.LastMessage.MessageSeq),
						ClientMsgNo:  conv.LastMessage.ClientMsgNo,
						FromUID:      conv.LastMessage.FromUID,
						ChannelID:    conv.ChannelID,
						ChannelType:  uint8(conv.ChannelType),
						Timestamp:    int32(conv.LastMessage.ServerTimestampMS / 1000),
						Payload:      conv.LastMessage.Payload,
					})
				}
			}
			results = append(results, item)
		}
		if listResp.More != 1 || listResp.NextCursor == nil {
			break
		}
		cursor = listResp.NextCursor
	}
	return results, nil
}
