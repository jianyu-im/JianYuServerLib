package config

import (
	"errors"
)

// errStreamNotSupported 流式消息是 v2 fork（jianyuim）专属特性，WuKongIM v3 尚无对应接口
var errStreamNotSupported = errors.New("WuKongIM v3 暂不支持流式消息，请改用整条消息发送")

// IMStreamStart 消息流开始
// v3 已无 /streammessage/start，返回明确错误让调用方（robot 等）降级为整条发送
func (c *Context) IMStreamStart(req MessageStreamStartReq) (string, error) {
	c.Warn("IMStreamStart：WuKongIM v3 暂不支持流式消息，调用方需降级为整条消息发送")
	return "", errStreamNotSupported
}

// IMStreamEnd 消息流结束
// v3 已无 /streammessage/end，与 IMStreamStart 同步降级
func (c *Context) IMStreamEnd(req MessageStreamEndReq) error {
	return errStreamNotSupported
}

type MessageStreamStartReq struct {
	Header      MsgHeader `json:"header"`        // 消息头
	ClientMsgNo string    `json:"client_msg_no"` // 客户端消息编号（相同编号，客户端只会显示一条）
	FromUID     string    `json:"from_uid"`      // 发送者UID
	ChannelID   string    `json:"channel_id"`    // 频道ID
	ChannelType uint8     `json:"channel_type"`  // 频道类型
	Payload     []byte    `json:"payload"`       // 消息内容
}

type MessageStreamEndReq struct {
	StreamNo    string `json:"stream_no"`    // 消息流编号
	ChannelID   string `json:"channel_id"`   // 频道ID
	ChannelType uint8  `json:"channel_type"` // 频道类型
}
