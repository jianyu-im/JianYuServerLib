package config

import (
	"errors"

	"github.com/jianyu-im/JianYuServerLib/pkg/network"
	"github.com/jianyu-im/JianYuServerLib/pkg/util"
)

// errStreamNotSupportedV3 流式消息是 v2 fork（jianyuim）专属特性，上游 WuKongIM v3 尚无对应接口
var errStreamNotSupportedV3 = errors.New("WuKongIM v3 暂不支持流式消息，请改用整条消息发送")

// IMStreamStart 消息流开始
// 返回流编号；v3 引擎下返回明确错误，调用方（robot 等）降级为整条发送
func (c *Context) IMStreamStart(req MessageStreamStartReq) (string, error) {
	if c.imEngineV3() {
		c.Warn("IMStreamStart：WuKongIM v3 暂不支持流式消息，调用方需降级为整条消息发送")
		return "", errStreamNotSupportedV3
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/streammessage/start", []byte(util.ToJson(req)), nil)
	if err != nil {
		return "", err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return "", err
	}
	var resultMap map[string]interface{}
	err = util.ReadJsonByByte([]byte(resp.Body), &resultMap)
	if err != nil {
		return "", err
	}
	if resultMap == nil {
		return "", errors.New("result is nil")
	}
	return resultMap["stream_no"].(string), nil
}

// IMStreamEnd 消息流结束；v3 引擎下与 IMStreamStart 同步降级
func (c *Context) IMStreamEnd(req MessageStreamEndReq) error {
	if c.imEngineV3() {
		return errStreamNotSupportedV3
	}
	resp, err := network.Post(c.cfg.WuKongIM.APIURL+"/streammessage/end", []byte(util.ToJson(req)), nil)
	if err != nil {
		return err
	}
	err = c.handlerIMError(resp)
	if err != nil {
		return err
	}
	return nil
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
