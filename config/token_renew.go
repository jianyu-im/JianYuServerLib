package config

import (
	"strconv"
	"sync"
	"time"

	"github.com/jianyu-im/JianYuServerLib/pkg/log"
	"go.uber.org/zap"
)

// token 滑动续期（2026-11-15）。
//
// 以前 token:<token> 只在登录时写一次 TTL（cache.tokenExpire，默认 30 天），之后不管用得多勤快都不续，
// 于是每个活跃用户每满 30 天就会被 401 退回登录页（客户投诉「账号自动退出了」）。
// 现在鉴权通过后，若该 token 剩余有效期已比满额少了 renewSlack，就把它连同 uidtoken 槽、usertokens 索引一起续回满额。
//
// 不变量：
//   - 只 EXPIRE 已存在且带 TTL 的 key：被吊销/删除的 token 不会被「续活」（EXPIRE 对不存在的 key 无效，且整段在 Lua 里原子执行）；
//     没有 TTL 的历史永久 token 保持原样，不顺手给它们加过期。
//   - uidtoken:<flag><uid> 只有值等于本 token 时才续，旧设备槽位不受影响。
//   - 后台管理员 token 不续（在 wkhttp.IsManagerRole 判定）。
//   - 进程内按 token 节流 renewCheckInterval，Redis 每 token 每小时至多一次 Lua；续期异步，Redis 故障只记日志不影响请求。
const (
	renewCheckInterval = time.Hour
	renewSlack         = 24 * time.Hour
	renewMemoMax       = 200000
)

const renewScript = `
local ttl = redis.call('TTL', KEYS[1])
if ttl <= 0 then return -1 end
local full = tonumber(ARGV[1])
if ttl > full - tonumber(ARGV[2]) then return 0 end
redis.call('EXPIRE', KEYS[1], full)
for _, f in ipairs({'0', '1', '2'}) do
  local k = ARGV[4] .. f .. ARGV[5]
  if redis.call('GET', k) == ARGV[3] then
    local t = redis.call('TTL', k)
    if t > 0 and t < full then redis.call('EXPIRE', k, full) end
  end
end
local it = redis.call('TTL', KEYS[2])
if it > 0 and it < full then redis.call('EXPIRE', KEYS[2], full) end
return 1
`

type tokenRenewer struct {
	ctx  *Context
	log  log.Log
	mu   sync.Mutex
	memo map[string]time.Time
	now  func() time.Time
	exec func(keys []string, args ...interface{}) (int64, error)
}

func (c *Context) tokenRenewer() *tokenRenewer {
	c.renewerOnce.Do(func() {
		r := &tokenRenewer{ctx: c, log: log.NewTLog("TokenRenew"), memo: map[string]time.Time{}, now: time.Now}
		r.exec = func(keys []string, args ...interface{}) (int64, error) {
			return c.GetRedisConn().Client().Eval(renewScript, keys, args...).Int64()
		}
		c.renewer = r
	})
	return c.renewer
}

func (r *tokenRenewer) due(token string) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.memo[token]; ok && now.Sub(last) < renewCheckInterval {
		return false
	}
	if len(r.memo) >= renewMemoMax {
		r.memo = map[string]time.Time{}
	}
	r.memo[token] = now
	return true
}

// Renew 实现 wkhttp.TokenRenewer
func (r *tokenRenewer) Renew(token string, uid string) {
	if token == "" || uid == "" || !r.due(token) {
		return
	}
	go r.renewNow(token, uid)
}

func (r *tokenRenewer) renewNow(token string, uid string) {
	cfg := r.ctx.cfg.Cache
	full := int64(cfg.TokenExpire / time.Second)
	if full <= 0 {
		return
	}
	res, err := r.exec(
		[]string{cfg.TokenCachePrefix + token, "usertokens:" + uid},
		strconv.FormatInt(full, 10), strconv.FormatInt(int64(renewSlack/time.Second), 10), token, cfg.UIDTokenCachePrefix, uid,
	)
	if err != nil {
		r.log.Warn("token续期失败", zap.String("uid", uid), zap.Error(err))
		return
	}
	if res == 1 {
		r.log.Debug("token已续期", zap.String("uid", uid))
	}
}
