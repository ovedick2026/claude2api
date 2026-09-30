package service

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"claude2api/internal/config"
	"claude2api/internal/repository"
	"claude2api/internal/utils"
)

// PublicAccount 是前端账号视图。
type PublicAccount struct {
	Email         string      `json:"email"`
	OrgUUID       string      `json:"org_uuid"`
	Status        string      `json:"status,omitempty"`
	DisabledUntil *time.Time  `json:"disabled_until"`           // 限流冷却截止时间，nil 表示无冷却
	DisableReason string      `json:"disable_reason,omitempty"` // 自动禁用原因：rate_limit=限流冷却，其他为错误摘要
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
	HasSession    bool        `json:"has_session"`
	Quota5h        int        `json:"quota_5h"`       // 5小时窗口已用 tokens（独立统计，5h 冷却到期归零）
	Quota5hLimit   int        `json:"quota_5h_limit"` // 5小时窗口额度上限 200k
	Quota7d        int        `json:"quota_7d"`       // 7天窗口已用 tokens（独立统计，7d 冷却到期归零）
	Quota7dLimit   int        `json:"quota_7d_limit"` // 7天窗口额度上限 2M
	Cooldown5hUntil *time.Time `json:"cooldown_5h_until"` // 5小时窗口冷却截止（前端标注触发窗口用）
	Cooldown7dUntil *time.Time `json:"cooldown_7d_until"` // 7天窗口冷却截止（前端标注触发窗口用）
	Opus5Remaining int `json:"opus5_remaining"` // claude-opus-5 当日剩余次数（每账号每日3次，自然日0点重置，仅显示）
	Opus5Limit     int `json:"opus5_limit"`
}

func SessionKey(account *repository.Account) string {
	if account == nil || account.Cookies == nil {
		return ""
	}
	return account.Cookies["sessionKey"]
}

// DisableReasonRateLimit 表示因限流（rate limit exceeded）被自动禁用，冷却结束自动恢复。
const DisableReasonRateLimit = "rate_limit"

// recoverWindowCooldowns 按窗口独立处理冷却到期（5h 与 7d 互不影响）：
// 5h 到期仅清 5h 冷却并把 5h 已用额度归零，7d 到期仅清 7d 冷却并把 7d 已用额度归零；
// 两个窗口均无未到期冷却时账号才恢复 active。返回是否有恢复/重置动作发生。
func recoverWindowCooldowns(email string) bool {
	changed := false
	repository.UpdateAccount(email, func(a *repository.Account) {
		if a.Status != repository.StatusCooldown {
			return
		}
		now := time.Now()
		if a.IsCooldown5hExpired(now) {
			a.Cooldown5hUntil = nil
			a.Quota5hUsed = 0
			changed = true
		}
		if a.IsCooldown7dExpired(now) {
			a.Cooldown7dUntil = nil
			a.Quota7dUsed = 0
			changed = true
		}
		if a.Cooldown5hUntil == nil && a.Cooldown7dUntil == nil {
			// 双窗口均无冷却：兼容旧数据（仅 DisabledUntil），其未到期则保持冷却。
			if a.DisabledUntil != nil && a.DisabledUntil.After(now) {
				return
			}
			a.Status = repository.StatusActive
			a.DisabledUntil = nil
			a.DisableReason = ""
			changed = true
			return
		}
		// 仍有窗口处于冷却：刷新聚合展示截止时间，账号保持冷却状态。
		a.DisabledUntil = a.AggregateCooldownUntil(now)
	})
	return changed
}

// AccountUsable 判断账号是否可用于调度；任一窗口冷却到期先按窗口独立恢复。
func AccountUsable(account *repository.Account) bool {
	if account == nil || SessionKey(account) == "" {
		return false
	}
	if account.Status == repository.StatusCooldown && recoverWindowCooldowns(account.Email) {
		if fresh := repository.AccountByEmail(account.Email); fresh != nil {
			*account = *fresh
		}
		slog.Info("[账号] 窗口冷却到期，已重置对应窗口额度", "email", account.Email)
	}
	return account.Status != repository.StatusExpired &&
		account.Status != repository.StatusCooldown &&
		account.Status != repository.StatusDisabled
}

// isRateLimitError 判断错误是否为上游限流（rate limit exceeded）。
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limit") ||
		strings.Contains(msg, "rate_limit") ||
		strings.Contains(msg, "ratelimit") ||
		strings.Contains(msg, "exceeded_limit") ||
		strings.Contains(msg, "too many requests")
}

// 限流窗口类型：对应上游 ratelimit 响应 windows 中的 "5h"/"7d" 标识。
// 5h 与 7d 两个窗口的额度独立统计、独立冷却、到期各自只重置各自当前额度。
const (
	rateLimitWindow5h = "5h"
	rateLimitWindow7d = "7d"
)

// parseRateLimitWindow 从限流错误信息解析本次限流触发窗口（"5h" 或 "7d"）。
// 上游 ratelimit 响应 message 中含 resetsAt（恢复时间）、representativeClaim
// （five_hour/seven_day）与 windows 对象（内含 "5h" 或 "7d" 键）。
// 判定依据（以 windows 标识为准）：windows 对象内第一个键为 "5h" 即 5 小时窗口限流，
// 为 "7d" 即 7 天窗口限流；windows 缺失或被截断时用 representativeClaim 兜底；
// 均无法识别时默认按 5h 处理。
func parseRateLimitWindow(err error) string {
	if err == nil {
		return rateLimitWindow5h
	}
	// message 是转义 JSON（内嵌反斜杠转义序列），先去掉反斜杠再匹配。
	msg := strings.ReplaceAll(err.Error(), "\\", "")
	if m := regexp.MustCompile(`(?i)"windows"\s*:\s*\{\s*"(5h|7d)"`).FindStringSubmatch(msg); len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	if regexp.MustCompile(`(?i)five[_ ]?hour`).MatchString(msg) {
		return rateLimitWindow5h
	}
	if regexp.MustCompile(`(?i)seven[_ ]?days?`).MatchString(msg) {
		return rateLimitWindow7d
	}
	return rateLimitWindow5h
}

// parseResetsAt 从错误信息提取上游冷却截止时间 resetsAt（支持 Unix 秒与 RFC3339）。
// 注意：message 是转义 JSON，resetsAt 后跟转义引号（反斜杠+引号）与冒号再接时间戳，
// 必须先去掉反斜杠再匹配，否则取值失败会错误退回默认 1 小时而不是上游真实恢复时间。
func parseResetsAt(err error) *time.Time {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), "\\", "")
	for _, pattern := range []string{
		`(?i)resets?[_ ]?at["':= ]+([0-9T:.+Zz-]+)`,
		`(?i)resets?[_ ]?(?:in|after)["':= ]+([0-9]+)`,
	} {
		m := regexp.MustCompile(pattern).FindStringSubmatch(msg)
		if len(m) < 2 {
			continue
		}
		if sec, perr := strconv.ParseInt(m[1], 10, 64); perr == nil && sec > 0 {
			t := time.Unix(sec, 0)
			return &t
		}
		if t, perr := time.Parse(time.RFC3339, m[1]); perr == nil {
			return &t
		}
	}
	return nil
}

// HandleRequestFailure 请求失败时按错误类型自动禁用账号：限流（429 或 rate limit exceeded）
// 按响应 windows 标识（5h/7d）判定触发窗口，取该响应 resetsAt 作为对应窗口冷却截止
// （缺失默认 1 小时），触发即冷却；两个窗口冷却互不覆盖，到期各自恢复并只重置各自额度；
// 其他错误置为 disabled 且不自动恢复。
func HandleRequestFailure(email string, reqErr error, statusCode int) {
	if email == "" {
		return
	}
	now := time.Now().UTC()
	if statusCode == 429 || isRateLimitError(reqErr) {
		until := parseResetsAt(reqErr)
		if until == nil || !until.After(now) {
			t := now.Add(time.Hour)
			until = &t
		}
		window := parseRateLimitWindow(reqErr)
		detail := ""
		if reqErr != nil {
			detail = utils.Truncate(reqErr.Error(), 200)
		}
		if repository.UpdateAccount(email, func(a *repository.Account) {
			a.Status = repository.StatusCooldown
			// 双窗口独立冷却：按触发窗口写对应截止时间，不覆盖另一窗口的冷却。
			if window == rateLimitWindow7d {
				a.Cooldown7dUntil = until
			} else {
				a.Cooldown5hUntil = until
			}
			// DisabledUntil 仅作聚合展示：两窗口中未到期的最晚截止。
			a.DisabledUntil = a.AggregateCooldownUntil(now)
			a.DisableReason = DisableReasonRateLimit
		}) {
			slog.Warn("[账号] 请求限流，账号禁用并进入冷却", "email", email, "window", window, "until", until.Format(time.RFC3339), "detail", detail)
		}
		return
	}
	detail := "unknown_error"
	if reqErr != nil {
		detail = utils.Truncate(reqErr.Error(), 200)
	}
	if repository.UpdateAccount(email, func(a *repository.Account) {
		a.Status = repository.StatusDisabled
		a.DisabledUntil = nil
		a.DisableReason = detail
	}) {
		slog.Warn("[账号] 请求失败，账号禁用且不自动恢复", "email", email, "code", statusCode, "detail", detail)
	}
}

// RecoverExpiredCooldowns 扫描全部账号，将限流冷却到期的窗口按窗口独立恢复，
// 各窗口到期仅重置各自当前额度（5h 到期清 Quota5hUsed，7d 到期清 Quota7dUsed，互不影响）。
func RecoverExpiredCooldowns() int {
	n := 0
	for _, account := range repository.LoadAccounts() {
		if account.Status == repository.StatusCooldown && recoverWindowCooldowns(account.Email) {
			slog.Info("[账号] 窗口冷却到期，已重置对应窗口额度并恢复", "email", account.Email)
			n++
		}
	}
	return n
}

func AccountByEmail(email string) *repository.Account {
	account := repository.AccountByEmail(email)
	if !AccountUsable(account) {
		return nil
	}
	return account
}

func PublicAccountView(account *repository.Account) *PublicAccount {
	if account == nil {
		return nil
	}
	// 双窗口额度直接读取账号上独立统计的已用 tokens（5h/7d 各自冷却到期归零），上限 5h=200k、7d=2M。
	return &PublicAccount{
		Email:           account.Email,
		OrgUUID:         account.OrgUUID,
		Status:          account.Status,
		DisabledUntil:   account.DisabledUntil,
		DisableReason:   account.DisableReason,
		CreatedAt:       account.CreatedAt,
		UpdatedAt:       account.UpdatedAt,
		HasSession:      SessionKey(account) != "",
		Opus5Remaining:  account.Opus5Remaining(),
		Opus5Limit:      repository.Opus5DailyLimit,
		Quota5h:         account.Quota5hUsed,
		Quota5hLimit:    repository.Quota5hLimit,
		Quota7d:         account.Quota7dUsed,
		Quota7dLimit:    repository.Quota7dLimit,
		Cooldown5hUntil: account.Cooldown5hUntil,
		Cooldown7dUntil: account.Cooldown7dUntil,
	}
}

// PublicAccounts 返回前端账号列表。
func PublicAccounts() []PublicAccount {
	src := repository.LoadAccounts()
	out := make([]PublicAccount, 0, len(src))
	for i := range src {
		out = append(out, *PublicAccountView(&src[i]))
	}
	return out
}

func StartAccountStatusMonitor() {
	go func() {
		for {
			time.Sleep(time.Duration(config.Get().StatusCheckIntervalSeconds) * time.Second)
			checkAccountStatuses()
		}
	}()
	// 本地冷却恢复循环：每10秒扫描一次到期冷却账号并恢复 active。
	// 纯本地数据库操作，不依赖上游接口与请求流量，
	// 避免无流量或上游查询失败时到期账号长期停留在 cooldown 状态、
	// 前端一直显示“冷却中 即将恢复”。
	go func() {
		for {
			time.Sleep(10 * time.Second)
			RecoverExpiredCooldowns()
		}
	}()
}

func RefreshAccount(email string) (*repository.Account, bool) {
	account := repository.AccountByEmail(email)
	sessionKey := SessionKey(account)
	if sessionKey == "" {
		return nil, false
	}

	client := NewClaudeAI(sessionKey, config.Get().Proxy, email)
	info, err := client.GetUserInfo()
	if repository.AccountByEmail(account.Email) == nil {
		return nil, false
	}
	if err != nil || info == nil || info.Email == "" {
		if err != nil && strings.Contains(err.Error(), "account_session_invalid") {
			if config.Get().RemoveInvalidAccount {
				repository.DeleteAccount(account.Email)
				slog.Warn("[账号刷新] 会话失效，已移除账号", "email", account.Email)
				return nil, true
			}
			repository.UpdateAccount(account.Email, func(a *repository.Account) { a.Status = "expired" })
			return repository.AccountByEmail(account.Email), false
		}
		repository.UpdateAccount(account.Email, func(a *repository.Account) { a.Status = "error" })
		slog.Warn("[账号刷新] 查询失败，保留账号", "email", account.Email, "err", err)
		return repository.AccountByEmail(account.Email), false
	}

	repository.UpdateAccount(account.Email, func(a *repository.Account) {
		a.Email, a.OrgUUID = info.Email, info.OrgUUID
		// 双窗口冷却独立处理：5h/7d 任一窗口冷却到期，仅重置该窗口冷却与当前额度；
		// 任一窗口仍在冷却中则保留 cooldown 状态（DisabledUntil 同步为剩余最晚截止），
		// 避免上游刷新把未到期窗口的冷却提前清零；两窗口均无冷却才恢复 active。
		now := time.Now()
		if a.Cooldown5hUntil != nil && !a.Cooldown5hUntil.After(now) {
			a.Cooldown5hUntil = nil
			a.Quota5hUsed = 0
		}
		if a.Cooldown7dUntil != nil && !a.Cooldown7dUntil.After(now) {
			a.Cooldown7dUntil = nil
			a.Quota7dUsed = 0
		}
		if a.Status == repository.StatusCooldown && a.InWindowCooldown(now) {
			a.DisabledUntil = a.AggregateCooldownUntil(now)
			return
		}
		a.Status = "active"
		a.DisabledUntil = nil
		a.DisableReason = ""
	})
	return repository.AccountByEmail(info.Email), false
}

func checkAccountStatuses() {
	for _, account := range repository.LoadAccounts() {
		if SessionKey(&account) == "" {
			continue
		}
		RefreshAccount(account.Email)
	}
}
