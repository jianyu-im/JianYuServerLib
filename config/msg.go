package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jianyu-im/JianYuServerLib/common"
	"github.com/jianyu-im/JianYuServerLib/pkg/network"
	"github.com/jianyu-im/JianYuServerLib/pkg/util"
	"github.com/sendgrid/rest"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// DeviceLevel 设备等级
type DeviceLevel uint8

const (
	// DeviceLevelSlave 从设备
	DeviceLevelSlave DeviceLevel = 0
	// DeviceLevelMaster 主设备
	DeviceLevelMaster DeviceLevel = 1
)

// DeviceFlag 设备类型
type DeviceFlag uint8

const (
	// APP APP
	APP DeviceFlag = iota
	// Web Web
	Web
	// PC在线
	PC
)

type Channel struct {
	ChannelID      string `json:"channel_id"`                // 频道ID
	ChannelType    uint8  `json:"channel_type"`              // 频道类型
	HistoryVisible int    `json:"history_visible,omitempty"` // 缺失成员索引重建时是否从频道起点同步（对应群「允许新成员查看历史消息」）
}

func (d DeviceFlag) Uint8() uint8 {
	return uint8(d)
}

// UpdateIMTokenReq 更新IM token的请求
type UpdateIMTokenReq struct {
	UID         string
	Token       string
	DeviceFlag  DeviceFlag
	DeviceLevel DeviceLevel
}

type UpdateTokenStatus int

const (
	UpdateTokenStatusSuccess UpdateTokenStatus = 200
	UpdateTokenStatusBan     UpdateTokenStatus = 19
)

// UpdateIMTokenResp 更新IM Token的返回参数
type UpdateIMTokenResp struct {
	Status UpdateTokenStatus `json:"status"` // 状态
}

// UpdateIMToken 更新IM的token
func (c *Context) UpdateIMToken(req UpdateIMTokenReq) (*UpdateIMTokenResp, error) {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/user/token", []byte(util.ToJson(map[string]interface{}{
		"uid":          req.UID,
		"token":        req.Token,
		"device_level": req.DeviceLevel,
		"device_flag":  req.DeviceFlag,
	})), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var result *UpdateIMTokenResp
	if err := util.ReadJsonByByte([]byte(resp.Body), &result); err != nil {
		return nil, err
	}
	return result, nil
}

// 退出用户指定的设备 deviceFlag -1 表示退出用户所有的设备
func (c *Context) QuitUserDevice(uid string, deviceFlag int) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/user/device_quit", []byte(util.ToJson(map[string]interface{}{
		"uid":         uid,
		"device_flag": deviceFlag,
	})), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		c.Error("IM服务错误！", zap.Error(err))
		return fmt.Errorf("IM服务返回状态[%d]失败！", resp.StatusCode)
	}
	return nil
}

// SendMessageBatch 给一批用户发送消息
func (c *Context) SendMessageBatch(req *MsgSendBatch) error {
	if c.IMV3Enabled() {
		return c.sendMessageBatchV3(req)
	}

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/message/sendbatch", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			resultMap, err := util.JsonToMap(resp.Body)
			if err != nil {
				return err
			}
			if resultMap != nil && resultMap["msg"] != nil {
				return fmt.Errorf("IM服务[SendMessageBatch]失败！ -> %s", resultMap["msg"])
			}
		}
		return fmt.Errorf("IM服务[SendMessageBatch]返回状态[%d]失败！", resp.StatusCode)
	}
	return nil

}

// SendMessage 发送消息
func (c *Context) SendMessage(req *MsgSendReq) error {
	_, err := c.SendMessageWithResult(req)
	return err
}

// SendMessage 发送消息
func (c *Context) SendMessageWithResult(req *MsgSendReq) (*MsgSendResp, error) {
	if c.IMV3Enabled() {
		req = adaptSendReqV3(req, c.cfg.Account.SystemUID)
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/message/send", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			resultMap, err := util.JsonToMap(resp.Body)
			if err != nil {
				return nil, err
			}
			if resultMap != nil && resultMap["msg"] != nil {
				return nil, fmt.Errorf("IM服务[SendMessage]失败！ -> %s", resultMap["msg"])
			}
		}
		// v3 的错误体是 {"error":"invalid request"}，没有 msg 字段，
		// 只打状态码等于没有任何线索（线上四万多条 400 全是一句"返回状态[400]失败"），把响应体带上
		return nil, fmt.Errorf("IM服务[SendMessage]返回状态[%d]失败！ -> %s", resp.StatusCode, strings.TrimSpace(resp.Body))
	} else {
		dataResult := gjson.Get(resp.Body, "data")
		if !dataResult.Exists() {
			// v3 把结果直接放在顶层（{"message_id":..,"message_seq":..,"reason":1}），没有 data 包裹。
			// 按 data 取会全拿到零值：机器人接口返回给第三方的 message_id 恒为 0。
			dataResult = gjson.Parse(resp.Body)
		}

		messageID := dataResult.Get("message_id").Int()
		messageSeq := dataResult.Get("message_seq").Int()
		clientMsgNo := dataResult.Get("client_msg_no").String()
		// v3 拒收消息时返回的仍是 200，只在 body 里给一个 reason（1=成功）。
		// 只看 HTTP 状态码会把「被拒收」当成功，消息静默消失、日志里没有任何痕迹——
		// 典型场景：禁言群的白名单不含系统号，入群提示被拒（reason=13），
		// 新成员在群里没有任何可见消息，客户端整个群都不出现。
		if c.IMV3Enabled() {
			if reason := dataResult.Get("reason"); reason.Exists() && reason.Int() != int64(msgReasonSuccessV3) {
				return nil, fmt.Errorf("IM服务[SendMessage]拒收消息！reason=%d(%s) channel=%s type=%d from=%s",
					reason.Int(), msgReasonTextV3(reason.Int()), req.ChannelID, req.ChannelType, req.FromUID)
			}
		}
		return &MsgSendResp{
			MessageID:   messageID,
			MessageSeq:  uint32(messageSeq),
			ClientMsgNo: clientMsgNo,
		}, nil
	}
}

// SendFriendApply 发送好友申请请求
func (c *Context) SendFriendApply(req *MsgFriendApplyReq) error {

	return c.SendMessage(&MsgSendReq{
		Header: MsgHeader{
			NoPersist: 0,
			RedDot:    0,
			SyncOnce:  1, // 只同步一次
		},
		ChannelID:   req.ToUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"apply_uid":  req.ApplyUID,
			"apply_name": req.ApplyName,
			"to_uid":     req.ToUID,
			"remark":     req.Remark,
			"token":      req.Token,
			"type":       common.FriendApply,
		})),
	})
}

// SendFriendSure 发送好友确认请求
func (c *Context) SendFriendSure(req *MsgFriendSureReq) error {
	return c.SendMessage(&MsgSendReq{
		Header: MsgHeader{
			NoPersist: 0,
			RedDot:    0,
			SyncOnce:  1, // 只同步一次
		},
		ChannelID:   req.ToUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload: []byte(util.ToJson(map[string]interface{}{
			"sure_uid":  req.FromUID,
			"sure_name": req.FromName,
			"to_uid":    req.ToUID,
			"content":   "你们已经是好友了，可以愉快的聊天了！",
			"type":      common.FriendSure,
		})),
	})
}

func (c *Context) SendFriendDelete(req *MsgFriendDeleteReq) error {
	return c.SendCMD(MsgCMDReq{
		ChannelID:   req.FromUID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		CMD:         common.CMDFriendDeleted,
		Param: map[string]interface{}{
			"uid": req.ToUID,
		},
	})
}

// IMCreateOrUpdateChannelInfo 修改或创建channel信息
func (c *Context) IMCreateOrUpdateChannelInfo(req *ChannelInfoCreateReq) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/info", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMAddSystemUids 添加系统成员
func (c *Context) IMAddSystemUids(uids []string) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/user/systemuids_add", []byte(util.ToJson(map[string]interface{}{
		"uids": uids,
	})), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMRemoveSystemUids 移除系统成员
func (c *Context) IMRemoveSystemUids(uids []string) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/user/systemuids_remove", []byte(util.ToJson(map[string]interface{}{
		"uids": uids,
	})), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMCreateOrUpdateChannel 请求IM创建或更新频道
func (c *Context) IMCreateOrUpdateChannel(req *ChannelCreateReq) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMBlacklistAdd 添加黑名单
func (c *Context) IMBlacklistAdd(req ChannelBlacklistReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/blacklist_add", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMBlacklistSet 设置黑名单
func (c *Context) IMBlacklistSet(req ChannelBlacklistReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/blacklist_set", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMBlacklistRemove 移除黑名单
func (c *Context) IMBlacklistRemove(req ChannelBlacklistReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/blacklist_remove", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMWhitelistAdd 添加白名单
func (c *Context) IMWhitelistAdd(req ChannelWhitelistReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/whitelist_add", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMWhitelistSet 白名单设置（覆盖旧的数据）
func (c *Context) IMWhitelistSet(req ChannelWhitelistReq) error {
	req.UIDs = withSystemUIDInWhitelist(req.UIDs, c.cfg.Account.SystemUID)

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/whitelist_set", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// withSystemUIDInWhitelist 白名单非空时兜底把系统号补进去。
//
// IM 侧的判定是「频道白名单非空 → 只有白名单里的人能发消息」，与是否全员禁言无关。
// 群禁言把白名单设成「群主+管理员」，系统号不在其中，于是入群提示、被移出群聊等系统消息
// 一律被 IM 拒收（v2 是 hasPermissionForSender 的 ReasonNotInWhitelist，v3 返回 200+reason=13），
// 新成员在群里没有任何可见消息，会话列表里就不会出现这个群。
//
// 空白名单表示「不启用白名单」（取消禁言就是把它清空），这种情况必须保持为空，
// 否则只剩系统号能发言，全群都被禁言了。
func withSystemUIDInWhitelist(uids []string, systemUID string) []string {
	if len(uids) == 0 || strings.TrimSpace(systemUID) == "" {
		return uids
	}
	for _, uid := range uids {
		if uid == systemUID {
			return uids
		}
	}
	return append(append(make([]string, 0, len(uids)+1), uids...), systemUID)
}

// IMWhitelistRemove 移除白名单
func (c *Context) IMWhitelistRemove(req ChannelWhitelistReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/whitelist_remove", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMAddSubscriber 请求IM创建频道
func (c *Context) IMAddSubscriber(req *SubscriberAddReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/subscriber_add", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("IM服务[IMAddSubscriber]返回状态[%d]失败！", resp.StatusCode)
	}
	return nil
}

// IMRemoveSubscriber 请求IM创建频道
func (c *Context) IMRemoveSubscriber(req *SubscriberRemoveReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/subscriber_remove", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMGetConversations 获取用户最近会话列表
func (c *Context) IMGetConversations(uid string) ([]*ConversationResp, error) {
	if c.IMV3Enabled() {
		return c.imGetConversationsV3(uid)
	}

	resp, err := network.Get(c.cfg.WuKongIM.APIURL+"/conversations", map[string]string{
		"uid": uid,
	}, nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var resps []*ConversationResp
	err = util.ReadJsonByByte([]byte(resp.Body), &resps)
	if err != nil {
		return nil, err
	}
	return resps, nil
}

// IMClearConversationUnread 清除用户某个频道的未读数
func (c *Context) IMClearConversationUnread(req ClearConversationUnreadReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversations/setUnread", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil
	}
	return c.handlerIMError(resp)
}

// IMDeleteConversation 删除最近会话
func (c *Context) IMDeleteConversation(req DeleteConversationReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversations/delete", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil
	}
	return c.handlerIMError(resp)
}

// IMSyncUserConversation 同步用户会话数据
func (c *Context) IMSyncUserConversation(uid string, version int64, msgCount int64, lastMsgSeqs string, larges []*Channel) ([]*SyncUserConversationResp, error) {
	if c.IMV3Enabled() {
		// v3 的会话列表按游标翻页，不吃 lastMsgSeqs；larges 在 v3 由 server 传入业务库中的
		// 全部有效群，作为目录校准集合（业务库已在群、IM 目录缺行时幂等补建），
		// 不再是 v2 的超大群语义。
		return c.imSyncUserConversationV3(uid, version, msgCount, larges)
	}

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/conversation/sync", []byte(util.ToJson(map[string]interface{}{
		"uid":           uid,
		"version":       version,
		"last_msg_seqs": lastMsgSeqs,
		"msg_count":     msgCount,
		"larges":        larges,
	})), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var conversations []*SyncUserConversationResp
	err = util.ReadJsonByByte([]byte(resp.Body), &conversations)
	if err != nil {
		return nil, err
	}
	return conversations, nil
}

// IMSyncUserConversationAck 同步用户会话数据回执
// func (c *Context) IMSyncUserConversationAck(uid string, cmdVersion int64) error {

// 	resp, err := network.Post(c.cfg.IMExtendURL+"/conversation/syncack", []byte(util.ToJson(map[string]interface{}{
// 		"uid":         uid,
// 		"cmd_version": cmdVersion,
// 	})), nil)
// 	if err != nil {
// 		return err
// 	}
// 	return c.handlerIMError(resp)

// }

// IMGetChannelMaxSeq
func (c *Context) IMGetChannelMaxSeq(channelID string, channelType uint8) (*ChannelMaxSeqResp, error) {
	if c.IMV3Enabled() {
		return c.imGetChannelMaxSeqV3(channelID, channelType)
	}

	resp, err := network.Get(c.cfg.WuKongIM.APIURL+"/channel/max_message_seq", map[string]string{
		"channel_id":   channelID,
		"channel_type": fmt.Sprintf("%d", channelType),
	}, nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var ChannelMaxSeqResp *ChannelMaxSeqResp
	err = util.ReadJsonByByte([]byte(resp.Body), &ChannelMaxSeqResp)
	if err != nil {
		return nil, err
	}
	return ChannelMaxSeqResp, nil
}

// IMGetChannelMaxSeqWithLoginUID 以指定用户身份查询频道最大 seq。
//
// v3 的 messagesync 带成员校验（个人频道还要用 loginUID 做归一化），
// 用真实成员身份查询才能拿到真值；v2 引擎下与 IMGetChannelMaxSeq 等价。
//
// 注意这里刻意不走 imV3SyncChannelMessages：那条路径会把「membership required」
// 伪装成空时间线，而本函数的调用方（入群隐藏历史的 channel_offset）拿到 0 会把
// 偏移写成 0=全量放开历史。成员校验失败必须原样抛错，让调用方换成员重试或回落。
func (c *Context) IMGetChannelMaxSeqWithLoginUID(channelID string, channelType uint8, loginUID string) (*ChannelMaxSeqResp, error) {
	if !c.IMV3Enabled() {
		return c.IMGetChannelMaxSeq(channelID, channelType)
	}
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
		return nil, err
	}
	if err = c.handlerIMError(resp); err != nil {
		return nil, err
	}
	var out SyncChannelMessageResp
	if err = util.ReadJsonByByte([]byte(resp.Body), &out); err != nil {
		return nil, err
	}
	var maxSeq uint32
	for _, msg := range out.Messages {
		if msg != nil && msg.MessageSeq > maxSeq {
			maxSeq = msg.MessageSeq
		}
	}
	if maxSeq == 0 && out.EndMessageSeq > maxSeq {
		maxSeq = out.EndMessageSeq
	}
	return &ChannelMaxSeqResp{MessageSeq: maxSeq}, nil
}

// IMGetWithChannelAndSeqs
func (c *Context) IMGetWithChannelAndSeqs(channelID string, channelType uint8, loginUID string, seqs []uint32) (*SyncChannelMessageResp, error) {
	if c.IMV3Enabled() {
		return c.imGetMessagesBySeqsV3(channelID, channelType, loginUID, seqs)
	}

	var req = map[string]interface{}{
		"channel_id":   channelID,
		"channel_type": channelType,
		"message_seqs": seqs,
		"login_uid":    loginUID,
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/messages", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var syncChannelMessageResp *SyncChannelMessageResp
	err = util.ReadJsonByByte([]byte(resp.Body), &syncChannelMessageResp)
	if err != nil {
		return nil, err
	}
	return syncChannelMessageResp, nil
}

// IMSyncChannelMessage 同步频道消息
func (c *Context) IMSyncChannelMessage(req SyncChannelMessageReq) (*SyncChannelMessageResp, error) {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesync", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		// 单聊的 membership required 可能是目录投影丢了一侧（真实历史在、行没了），
		// 先触发引擎补行再重试一次；补不上才降级
		if c.IMV3Enabled() && req.ChannelType == common.ChannelTypePerson.Uint8() &&
			isMembershipRequiredErrV3(err) && c.repairPersonMembershipV3(req.LoginUID, req.ChannelID) {
			time.Sleep(300 * time.Millisecond) // 等 raft apply 落盘
			if retryResp, retryErr := network.Post(c.cfg.WuKongIM.APIURL+"/channel/messagesync", []byte(util.ToJson(req)), nil); retryErr == nil {
				if retryErr = c.handlerIMError(retryResp); retryErr == nil {
					resp, err = retryResp, nil
				}
			}
		}
	}
	if err != nil {
		// v3 里「频道不存在」是以成员校验失败的形式返回的，降级成空时间线，
		// 否则客户端打开系统号单聊或废弃群时会直接报错打不开
		if c.IMV3Enabled() && isChannelAbsentErrV3(err) {
			c.Warn("v3频道无时间线，按空结果返回", zap.String("login_uid", req.LoginUID), zap.String("channel_id", req.ChannelID), zap.Uint8("channel_type", req.ChannelType), zap.Error(err))
			return emptySyncChannelMessageRespV3(req.StartMessageSeq, req.EndMessageSeq, req.PullMode), nil
		}
		return nil, err
	}
	var syncChannelMessageResp *SyncChannelMessageResp
	err = util.ReadJsonByByte([]byte(resp.Body), &syncChannelMessageResp)
	if err != nil {
		return nil, err
	}
	if c.IMV3Enabled() && syncChannelMessageResp != nil {
		// v3 的响应体不带 pull_mode，反序列化后恒为 0（PullModeDown），按请求回填保持 v2 契约
		syncChannelMessageResp.PullMode = req.PullMode
	}
	return syncChannelMessageResp, nil
}

// IMSyncMessage 同步IM消息
func (c *Context) IMSyncMessage(req *MsgSyncReq) ([]*MessageResp, error) {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/message/sync", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var resps []*MessageResp
	err = util.ReadJsonByByte([]byte(resp.Body), &resps)
	if err != nil {
		return nil, err
	}
	return resps, nil
}

// IMSyncMessageAck 同步IM消息回执
func (c *Context) IMSyncMessageAck(req *SyncackReq) error {

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/message/syncack", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMDeleteMessage 删除IM消息
// func (c *Context) IMDeleteMessage(req *MessageDeleteReq) error {

// 	resp, err := network.Post(c.cfg.IMExtendURL+"/message/delete", []byte(util.ToJson(req)), nil)
// 	if err != nil {
// 		return err
// 	}
// 	return c.handlerIMError(resp)
// }

// IMRevokeMessage 撤回IM消息
func (c *Context) IMRevokeMessage(req *MessageRevokeReq) error {
	if c.IMV3Enabled() {
		// v3 没有 /message/revoke 路由，打过去必然 404。撤回请走 SendRevoke 的 messageRevoke CMD 路径。
		return errors.New("v3引擎不支持/message/revoke，撤回请使用SendRevoke")
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/message/revoke", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMDelChannel 删除频道
func (c *Context) IMDelChannel(req *ChannelDeleteReq) error {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/channel/delete", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	return c.handlerIMError(resp)
}

// IMSearchUserMessages 搜索用户消息
func (c *Context) IMSearchUserMessages(req *SearchUserMessageReq) (*SearchUserMessageResp, error) {
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/plugins/wk.plugin.search/usersearch", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	println(resp.Body)
	var messageResp *SearchUserMessageResp
	err = util.ReadJsonByByte([]byte(resp.Body), &messageResp)
	if err != nil {
		return nil, err
	}
	return messageResp, nil
}

// IMGetWithMessageID 根据消息ID获取消息详情
func (c *Context) IMSearchMessages(req *MsgSearchReq) (*SyncChannelMessageResp, error) {
	if c.IMV3Enabled() {
		return c.imSearchMessagesV3(req)
	}

	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/messages", []byte(util.ToJson(req)), nil)
	if err != nil {
		return nil, err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return nil, err
	}
	var messageResp *SyncChannelMessageResp
	err = util.ReadJsonByByte([]byte(resp.Body), &messageResp)
	if err != nil {
		return nil, err
	}
	return messageResp, nil
}

// SendRevoke 发送撤回消息
func (c *Context) SendRevoke(req *MsgRevokeReq) error {

	return c.SendCMD(MsgCMDReq{
		FromUID:     req.FromUID,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		CMD:         "messageRevoke",
		Param: map[string]interface{}{
			"message_id": fmt.Sprintf("%d", req.MessageID),
		},
	})
}

// SendCMD 发送CMD消息
func (c *Context) SendCMD(req MsgCMDReq) error {

	contentMap := map[string]interface{}{
		"cmd":  req.CMD,
		"type": common.CMD,
	}
	if req.Param != nil {
		contentMap["param"] = req.Param
	}
	// v3 的群 CMD 会被改写成定向投递（见 sendGroupCMDV3），到客户端的帧上带的是
	// 按订阅者算出来的合成频道 id，不再是群号。安卓 SDK 在 param 没带频道时是拿帧上的
	// channel_id 兜底的（CMDManager.handleCMD），拿到合成 id 就会作用到一个不存在的会话上。
	// 这里把真实群号写进 CMD 顶层——安卓的兜底顺序是 param > CMD 顶层 > 帧，正好覆盖。
	// 只对群频道这么做：单聊的频道 id 是「对端视角」的，服务端这边的值填过去反而是错的。
	if c.IMV3Enabled() && req.ChannelType == common.ChannelTypeGroup.Uint8() && strings.TrimSpace(req.ChannelID) != "" {
		contentMap["channel_id"] = req.ChannelID
		contentMap["channel_type"] = req.ChannelType
	}
	//默认不存储
	var noPersist = 1
	//if req.NoPersist {
	//	noPersist = 1
	//}
	setting := Setting{
		NoUpdateConversation: true,
	}

	contentBytes := []byte(util.ToJson(contentMap))

	sendReq := &MsgSendReq{
		Header: MsgHeader{
			NoPersist: noPersist,
			RedDot:    0,
			SyncOnce:  1,
		},
		Setting:     setting.ToUint8(),
		FromUID:     req.FromUID,
		ChannelID:   req.ChannelID,
		ChannelType: req.ChannelType,
		Subscribers: req.Subscribers,
		Payload:     contentBytes,
	}
	if c.IMV3Enabled() {
		// v3 的群 CMD 走频道投递查不到订阅者，改成按成员定向投递，详见 sendGroupCMDV3
		if handled, err := c.sendGroupCMDV3(sendReq); handled {
			return err
		}
	}
	return c.SendMessage(sendReq)
}

func (c *Context) SendTyping(channelID string, channelType uint8, fromUID string) error {
	// 发送输入中的命令
	err := c.SendCMD(MsgCMDReq{
		NoPersist:   true,
		CMD:         common.CMDTyping,
		ChannelID:   channelID,
		ChannelType: channelType,
		Param: map[string]interface{}{
			"from_uid":     fromUID,
			"channel_id":   channelID,
			"channel_type": channelType,
		},
	})
	return err
}

func (c *Context) handlerIMError(resp *rest.Response) error {
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusBadRequest {
			resultMap, err := util.JsonToMap(resp.Body)
			if err != nil {
				return err
			}
			if resultMap != nil && resultMap["msg"] != nil {
				return fmt.Errorf("IM服务失败！ -> %s", resultMap["msg"])
			}
		}
		return fmt.Errorf("IM服务返回状态[%d]失败！", resp.StatusCode)
	}
	return nil
}

// ---------- req  ----------

type PullMode int

const (
	PullModeDown PullMode = iota
	PullModeUp
)

// SyncChannelMessageReq 同步频道消息请求
type SyncChannelMessageReq struct {
	LoginUID        string   `json:"login_uid"`
	DeviceUUID      string   `json:"device_uuid"`
	ChannelID       string   `json:"channel_id"`
	ChannelType     uint8    `json:"channel_type"`
	StartMessageSeq uint32   `json:"start_message_seq"` // 开始序列号
	EndMessageSeq   uint32   `json:"end_message_seq"`   // 结束序列号
	Limit           int      `json:"limit"`             // 每次同步数量限制
	PullMode        PullMode `json:"pull_mode"`         // 拉取模式
}

// SyncChannelMessageResp 同步频道消息返回
type SyncChannelMessageResp struct {
	StartMessageSeq uint32         `json:"start_message_seq"` // 开始序列号
	EndMessageSeq   uint32         `json:"end_message_seq"`   // 结束序列号
	PullMode        PullMode       `json:"pull_mode"`         // 拉取模式
	Messages        []*MessageResp `json:"messages"`          // 消息数据
}

// ChannelMaxSeqResp 频道最大序列号返回
type ChannelMaxSeqResp struct {
	MessageSeq uint32 `json:"message_seq"` // 最大序列号
}

// ClearConversationUnreadReq 清除用户某个频道未读数请求
type ClearConversationUnreadReq struct {
	UID         string `json:"uid"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	Unread      int    `json:"unread"`
	MessageSeq  uint32 `json:"message_seq"`
}

// SyncackReq 同步回执请求
type SyncackReq struct {
	// 用户uid
	UID string `json:"uid"`
	// 最后一次同步的message_seq
	LastMessageSeq uint32 `json:"last_message_seq"`
}

func (s SyncackReq) String() string {
	return fmt.Sprintf("UID: %s LastMessageSeq: %d", s.UID, s.LastMessageSeq)
}

// Check 检查参数输入
func (s SyncackReq) Check() error {
	if strings.TrimSpace(s.UID) == "" {
		return errors.New("用户UID不能为空！")
	}
	if s.LastMessageSeq == 0 {
		return errors.New("最后一次messageSeq不能为0！")
	}
	return nil
}

// IMSOnlineStatus 获取指定用户的在线状态
func (c *Context) IMSOnlineStatus(uids []string) ([]*OnlinestatusResp, error) {
	if c.cfg.Test {
		c.Info("获取指定用户的在线状态", zap.String("req", util.ToJson(uids)))
		return nil, nil
	}
	if len(uids) == 0 {
		// v3 对空 uids 返回对象 {"status":200} 而不是数组，反序列化必炸；空入参没有查询意义，直接短路
		return make([]*OnlinestatusResp, 0), nil
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/user/onlinestatus", []byte(util.ToJson(uids)), nil)
	if err != nil {
		return nil, err
	}
	if err := c.handlerIMError(resp); err != nil {
		return nil, err
	}
	var resps []*OnlinestatusResp
	err = util.ReadJsonByByte([]byte(resp.Body), &resps)
	if err != nil {
		return nil, err
	}
	return resps, nil
}

// OnlinestatusResp 在线状态返回
type OnlinestatusResp struct {
	UID         string `json:"uid"`          // 在线用户uid
	DeviceFlag  uint8  `json:"device_flag"`  // 设备标记 0. APP 1.web
	LastOffline int    `json:"last_offline"` // 最后一次离线时间
	Online      int    `json:"online"`       // 是否在线
}

// MessageDeleteReq 删除消息请求
type MessageDeleteReq struct {
	UID         string   `json:"uid"`          // 频道ID
	ChannelID   string   `json:"channel_id"`   // 频道ID
	ChannelType uint8    `json:"channel_type"` // 频道类型
	MessageIDs  []uint64 `json:"message_ids"`  // 消息ID集合 （如果all=1 则此字段无效）
}

// MessageRevokeReq 消息撤回请求
type MessageRevokeReq struct {
	ChannelID   string   `json:"channel_id"`   // 频道ID
	ChannelType uint8    `json:"channel_type"` // 频道类型
	MessageIDs  []uint64 `json:"message_ids"`  // 指定需要撤回的消息
}

// ChannelDeleteReq 删除频道请求
type ChannelDeleteReq struct {
	ChannelID   string `json:"channel_id"`   // 频道ID
	ChannelType uint8  `json:"channel_type"` // 频道类型
}

// MsgRevokeReq 撤回消息请求
type MsgRevokeReq struct {
	FromUID      string `json:"from_uid"`
	Operator     string `json:"operator"`      // 操作者uid
	OperatorName string `json:"operator_name"` // 操作者名称
	ChannelID    string `json:"channel_id"`    // 频道ID
	ChannelType  uint8  `json:"channel_type"`  // 频道类型
	MessageID    int64  `json:"message_id"`    // 消息ID
}

// SearchUserMessageReq 用户消息搜索
type SearchUserMessageReq struct {
	UID          string                 `json:"uid"`           // 当前用户uid（限制搜索指定用户的消息）
	Payload      map[string]interface{} `json:"payload"`       // 消息payload，支持搜索自定义字段
	PayloadTypes []int                  `json:"payload_types"` // 消息类型搜索
	FromUID      string                 `json:"from_uid"`      // 发送者uid
	ChannelID    string                 `json:"channel_id"`    // 频道ID
	ChannelType  uint8                  `json:"channel_type"`  // 频道类型
	Topic        string                 `json:"topic"`         // 根据topic搜索
	Limit        int                    `json:"limit"`         // 查询限制数量
	Page         int                    `json:"page"`          // 页码，分页使用，默认为1
	StartTime    int64                  `json:"start_time"`    //  消息时间（开始）
	EndTime      int64                  `json:"end_time"`      // 消息时间（结束，结果不包含end_time）
	Highlights   []string               `json:"highlights"`    // 搜索关键字是否高亮显示， 比如payload.content="你是北京大学的吗" 搜索关键字:"北京" 那么highlights设置为["payload.content"] 这样payload.content返回的内容为带上mark标签为："你是<mark>北京</mark>大学的吗"
}

// MsgSearchReq 消息查询请求
type MsgSearchReq struct {
	LoginUID     string   `json:"login_uid"`      // 登录者UID
	ChannelID    string   `json:"channel_id"`     // 频道ID
	ChannelType  uint8    `json:"channel_type"`   // 频道类型
	MessageSeqs  []uint32 `json:"message_seqs"`   // 消息序列号
	MessageIds   []int64  `json:"message_ids"`    // 消息ids
	ClientMsgNos []string `json:"client_msg_nos"` // 客户端消息唯一编号
}

// UserBaseVo 用户基础信息
type UserBaseVo struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// MsgSendReq 发送消息请求
type MsgSendReq struct {
	Header      MsgHeader `json:"header"`       // 消息头
	Setting     uint8     `json:"setting"`      // setting
	FromUID     string    `json:"from_uid"`     // 模拟发送者的UID
	ChannelID   string    `json:"channel_id"`   // 频道ID
	ChannelType uint8     `json:"channel_type"` // 频道类型
	StreamNo    string    `json:"stream_no"`    // 消息流号
	Subscribers []string  `json:"subscribers"`  // 订阅者 如果此字段有值，表示消息只发给指定的订阅者
	Payload     []byte    `json:"payload"`      // 消息内容
}

type MsgSendResp struct {
	MessageID   int64  `json:"message_id"`    // 消息ID
	ClientMsgNo string `json:"client_msg_no"` // 客户端消息唯一编号
	MessageSeq  uint32 `json:"message_seq"`   // 消息序号
}

// MsgSendBatch 给一批用户发送消息请求
type MsgSendBatch struct {
	Header      MsgHeader `json:"header"`      // 消息头
	FromUID     string    `json:"from_uid"`    // 模拟发送者的UID
	Subscribers []string  `json:"subscribers"` // 订阅者 如果此字段有值，表示消息只发给指定的订阅者
	Payload     []byte    `json:"payload"`     // 消息内容
}

func (m *MsgSendReq) String() string {
	return fmt.Sprintf("ChannelID:%s ChannelType:%d Payload:%s", m.ChannelID, m.ChannelType, string(m.Payload))
}

// MsgFriendApplyReq 好友申请
type MsgFriendApplyReq struct {
	ApplyUID  string `json:"apply_uid"`  // 发起申请人的uid
	ApplyName string `json:"apply_name"` // 发起申请人的名字
	ToUID     string `json:"to_uid"`     // 接收者
	Remark    string `json:"remark"`     // 申请备注
	Token     string `json:"token"`      // 凭证
}

// MsgFriendSureReq 确认好友申请
type MsgFriendSureReq struct {
	ToUID    string `json:"to_uid"`    // 接收好友申请的人uid
	FromUID  string `json:"from_uid"`  // 发起申请人的uid
	FromName string `json:"from_name"` // 发起申请人的名字
}

// MsgFriendDeleteReq 好友删除
type MsgFriendDeleteReq struct {
	FromUID string `json:"from_uid"` // 删除人的uid
	ToUID   string `json:"to_uid"`   // 被删除的好友uid
}

// MsgGroupMemberAddReq 添加群成员
type MsgGroupMemberAddReq struct {
	Operator     string        `json:"operator"`      // 操作者uid
	OperatorName string        `json:"operator_name"` // 操作者名称
	GroupNo      string        `json:"group_no"`      // 群编号
	Members      []*UserBaseVo `json:"members"`       // 邀请成员
}

// CMDGroupAvatarUpdateReq 群头像更新请求
type CMDGroupAvatarUpdateReq struct {
	GroupNo string   `json:"group_no"` // 群编号
	Members []string `json:"members"`  // 成员uids
}

// MsgCMDReq CMD消息请求
type MsgCMDReq struct {
	NoPersist   bool                   `json:"-"`            // 是否需要存储
	FromUID     string                 `json:"from_uid"`     // 模拟发送者的UID
	ChannelID   string                 `json:"channel_id"`   // 频道ID
	ChannelType uint8                  `json:"channel_type"` // 频道类型
	Subscribers []string               `json:"subscribers"`  // 订阅者 如果此字段有值，表示消息只发给指定的订阅者
	CMD         string                 `json:"cmd"`          // 操命令
	Param       map[string]interface{} `json:"param"`        // 命令参数
}

// MsgSyncReq 消息同步请求
type MsgSyncReq struct {
	UID        string `json:"uid"`         // 谁的消息
	MessageSeq uint32 `json:"message_seq"` // 客户端最大消息序列号
	Limit      int    `json:"limit"`       // 消息数量限制
}

// MsgHeader 消息头
type MsgHeader struct {
	NoPersist int `json:"no_persist"` // 是否不持久化
	RedDot    int `json:"red_dot"`    // 是否显示红点
	SyncOnce  int `json:"sync_once"`  // 此消息只被同步或被消费一次(1表示消息将走写模式) ，特别注意：sync_once=1表示写扩散 sync_once=0表示读扩散 写扩散的messageSeq和读扩散messageSeq来源不一样
}

func (h MsgHeader) String() string {
	return fmt.Sprintf("NoPersist:%d RedDot:%d SyncOnce:%d", h.NoPersist, h.RedDot, h.SyncOnce)
}

// SyncUserConversationResp 最近会话离线返回
type SyncUserConversationResp struct {
	ChannelID       string         `json:"channel_id"`         // 频道ID
	ChannelType     uint8          `json:"channel_type"`       // 频道类型
	Unread          int            `json:"unread"`             // 未读消息
	Timestamp       int64          `json:"timestamp"`          // 最后一次会话时间
	LastMsgSeq      int64          `json:"last_msg_seq"`       // 最后一条消息seq
	LastClientMsgNo string         `json:"last_client_msg_no"` // 最后一条客户端消息编号
	OffsetMsgSeq    int64          `json:"offset_msg_seq"`     // 偏移位的消息seq
	Version         int64          `json:"version"`            // 数据版本
	Recents         []*MessageResp `json:"recents"`            // 最近N条消息
}

// SyncUserConversationRespWrap SyncUserConversationRespWrap
type SyncUserConversationRespWrap struct {
	Conversations []*SyncUserConversationResp `json:"conversations"`
	CMDVersion    int64                       `json:"cmd_version"` // 最新cmd版本号
	CMDs          []*CMDResp                  `json:"cmds"`        // cmd集合
}

// CMDResp CMDResp
type CMDResp struct {
	CMD   string      `json:"cmd"`
	Param interface{} `json:"param"`
}

// Setting Setting
type Setting struct {
	Receipt              bool // 消息已读回执，此标记表示，此消息需要已读回执
	NoUpdateConversation bool // 不更新最近会话
	Signal               bool // 是否signal加密
}

// ToUint8 ToUint8
func (s Setting) ToUint8() uint8 {
	return uint8(encodeBool(s.Receipt)<<7 | encodeBool(s.NoUpdateConversation)<<6 | encodeBool(s.Signal)<<5)
}

// SettingFromUint8 SettingFromUint8
func SettingFromUint8(v uint8) Setting {
	s := Setting{}
	s.Receipt = (v >> 7 & 0x01) > 0
	s.NoUpdateConversation = (v >> 6 & 0x01) > 0
	s.Signal = (v >> 5 & 0x01) > 0
	return s
}
func encodeBool(b bool) (i int) {
	if b {
		i = 1
	}
	return
}

// MessageResp 消息
type MessageResp struct {
	Header       MsgHeader         `json:"header"`              // 消息头
	Setting      uint8             `json:"setting"`             // 设置
	MessageIDStr string            `json:"message_idstr"`       // 消息字符串ID
	MessageID    int64             `json:"message_id"`          // 服务端的消息ID(全局唯一)
	MessageSeq   uint32            `json:"message_seq"`         // 消息序列号 （用户唯一，有序递增）
	ClientMsgNo  string            `json:"client_msg_no"`       // 客户端消息唯一编号
	Expire       uint32            `json:"expire"`              // 消息过期时间
	FromUID      string            `json:"from_uid"`            // 发送者UID
	ToUID        string            `json:"to_uid"`              // 接受者uid
	ChannelID    string            `json:"channel_id"`          // 频道ID
	ChannelType  uint8             `json:"channel_type"`        // 频道类型
	Timestamp    int32             `json:"timestamp"`           // 服务器消息时间戳(10位，到秒)
	Payload      []byte            `json:"payload"`             // 消息内容
	StreamNo     string            `json:"stream_no,omitempty"` // 流编号
	Streams      []*StreamItemResp `json:"streams,omitempty"`   // 消息流
	// ReplyCount    int            `json:"reply_count,omitempty"`     // 回复集合
	// ReplyCountSeq string         `json:"reply_count_seq,omitempty"` // 回复数量seq
	// ReplySeq      string         `json:"reply_seq,omitempty"`       // 回复seq
	// Reactions     []ReactionResp `json:"reactions,omitempty"`       // 回应数据
	IsDeleted   int `json:"is_deleted"`   // 是否已删除
	VoiceStatus int `json:"voice_status"` // 语音状态 0.未读 1.已读

	payloadMap map[string]interface{}
}

// GetPayloadMap GetPayloadMap
func (m *MessageResp) GetPayloadMap() (map[string]interface{}, error) {
	if m.payloadMap == nil {
		var payloadMap map[string]interface{}
		if err := util.ReadJsonByByte(m.Payload, &payloadMap); err != nil {
			return nil, err
		}
		m.payloadMap = payloadMap
	}
	return m.payloadMap, nil
}

// GetContentType 消息正文类型
func (m *MessageResp) GetContentType() int {
	payloadMap, err := m.GetPayloadMap()
	if err != nil {
		return 0
	}
	contentTypeInt64, _ := payloadMap["type"].(json.Number).Int64()
	return int(contentTypeInt64)
}

type StreamItemResp struct {
	StreamSeq   uint32 `json:"stream_seq"`    // 流序号
	ClientMsgNo string `json:"client_msg_no"` // 客户端消息唯一编号
	Blob        []byte `json:"blob"`          // 消息内容
}

// ReactionResp 回应返回
type ReactionResp struct {
	Seq   string     `json:"seq"`   // 回复序列号
	Users []UserResp `json:"users"` // 回应用户集合
	Emoji string     `json:"emoji"` // 回应的emoji
	Count int        `json:"count"` // 回应数量
}

// UserResp UserResp
type UserResp struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// ConversationResp 最近会话返回数据
type ConversationResp struct {
	ChannelID   string       `json:"channel_id"`   // 频道ID
	ChannelType uint8        `json:"channel_type"` // 频道类型
	Unread      int64        `json:"unread"`       // 未读数
	Timestamp   int64        `json:"timestamp"`    // 最后一次会话时间戳
	LastMessage *MessageResp `json:"last_message"` // 最后一条消息
}

// ChannelCreateReq 频道创建请求
type ChannelCreateReq struct {
	ChannelID   string   `json:"channel_id"`   // 频道ID
	ChannelType uint8    `json:"channel_type"` // 频道类型
	Ban         int      `json:"ban"`          // 是否被封禁（被封后 任何人都不能发消息，包括创建者）
	Large       int      `json:"large"`        // 是否是超大群（一般建议500或1000成员以上设置为超大群，超大群，注意：超大群不会维护最近会话数据）
	Subscribers []string `json:"subscribers"`  // 订阅者
}

// ChannelInfoCreateReq
type ChannelInfoCreateReq struct {
	ChannelID   string `json:"channel_id"`   // 频道ID
	ChannelType uint8  `json:"channel_type"` // 频道类型
	Ban         int    `json:"ban"`          // 是否封禁
	Large       int    `json:"large"`        // 是否大群
}

// ChannelReq ChannelReq
type ChannelReq struct {
	ChannelID   string `json:"channel_id"`   // 频道ID
	ChannelType uint8  `json:"channel_type"` // 频道类型
}

// DeleteConversationReq  DeleteConversationReq
type DeleteConversationReq struct {
	UID         string `json:"uid"`
	ChannelID   string `json:"channel_id"`   // 频道ID
	ChannelType uint8  `json:"channel_type"` // 频道类型
}

// ChannelBlacklistReq 黑名单
type ChannelBlacklistReq struct {
	ChannelReq
	UIDs []string `json:"uids"` // 黑名单用户
}

// ChannelWhitelistReq 白名单
type ChannelWhitelistReq struct {
	ChannelReq
	UIDs []string `json:"uids"` // 白名单用户
}

// SubscriberAddReq 添加订阅请求
type SubscriberAddReq struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	Reset       int      `json:"reset"` // 是否重置订阅者 （0.不重置 1.重置），选择重置，将删除原来的所有成员
	Subscribers []string `json:"subscribers"`
	// HistoryVisible 新成员是否可见入群前的历史消息（对应群设置「允许新成员查看历史消息」）。
	// v3 的 messagesync 在服务端按成员 JoinSeq 过滤历史，不传则新成员永远拉不到入群前的消息。
	HistoryVisible int `json:"history_visible,omitempty"`
}

// SubscriberRemoveReq 移除订阅请求
type SubscriberRemoveReq struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	Subscribers []string `json:"subscribers"`
}

// UpdateSearchMessageReq 修改搜索消息
type UpdateSearchMessageReq struct {
	ChannelID  string   `json:"channel_id"`
	MessageIDs []string `json:"message_ids"`
}

// SearchUserMessageResp 搜索用户消息结果
type SearchUserMessageResp struct {
	Total    int64          `json:"total"`    // 消息总量
	Limit    int            `json:"limit"`    // 查询数量
	Page     int            `json:"page"`     // 当前页码
	Messages []*MessageResp `json:"messages"` // 消息
}
