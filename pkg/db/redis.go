package db

import "github.com/jianyu-im/JianYuServerLib/pkg/redis"

func NewRedis(addr string, password string) *redis.Conn {
	return redis.New(addr, password)
}
