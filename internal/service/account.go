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
	Email         string     `json:"email"`
	OrgUUID       string     `json:"org_uuid"`
	Status        string     `json:"status,omitempty"`
	DisabledUntil *time.Time `json:"disabled_until"`           // 限流冷却截止时间，nil 表示无冷却
	DisableReason string     `json:"disable_reason,omitempty"` // 自动禁用原因：rate_limit=限流冷却，其他为错误摘要
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	HasSession    bool       `json:"has_session"`
}

func SessionKey(account *repository.Account) string {
	if account == nil || account.Cookies == nil {
		return ""
	}
	return account.Cookies["sessionKey"]
}

// DisableReasonRateLimit 表示因限流（rate limit exceeded）被自动禁用，冷却结束自动恢复。
const DisableReasonRateLimit = "rate_limit"

// AccountUsable 判断账号是否可用于调度；限流冷却到期的账号先自动恢复为 active。
func AccountUsable(account *repository.Account) bool {
	if account == nil || SessionKey(account) == "" {
		return false
	}
	if account.IsCooldownExpired() {
		recovered := false
		repository.UpdateAccount(account.Email, func(a *repository.Account) {
			if a.IsCooldownExpired() {
				a.Status = repository.StatusActive
				a.DisabledUntil = nil
				a.DisableReason = ""
				recovered = true
			}
		})
		if recovered {
			slog.Info("[账号] 限流冷却结束，自动恢复启用", "email", account.Email)
			account.Status = repository.StatusActive
			account.DisabledUntil = nil
			account.DisableReason = ""
		}
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

// parseResetsAt 从错误信息提取上游冷却截止时间 resetsAt（支持 RFC3339 与 Unix 秒）。
func parseResetsAt(err error) *time.Time {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, pattern := range []string{
		`(?i)resets?[_ ]?at["':= ]+([0-9T:.+Zz-]+)`,
		`(?i)resets?[_ ]?(?:in|after)["':= ]+([0-9]+)`,
	} {
		m := regexp.MustCompile(pattern).FindStringSubmatch(msg)
		if len(m) < 2 {
			continue
		}
		if t, perr := time.Parse(time.RFC3339, m[1]); perr == nil {
			return &t
		}
		if sec, perr := strconv.ParseInt(m[1], 10, 64); perr == nil && sec > 0 {
			t := time.Unix(sec, 0)
			return &t
		}
	}
	return nil
}

// HandleRequestFailure 请求失败时按错误类型自动禁用账号：限流（429 或 rate limit exceeded）进入 cooldown 冷却，DisabledUntil 取上游 resetsAt（缺失默认 1 小时），到期自动恢复；其他错误置为 disabled 且不自动恢复。
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
		detail := ""
		if reqErr != nil {
			detail = utils.Truncate(reqErr.Error(), 200)
		}
		if repository.UpdateAccount(email, func(a *repository.Account) {
			a.Status = repository.StatusCooldown
			a.DisabledUntil = until
			a.DisableReason = DisableReasonRateLimit
		}) {
			slog.Warn("[账号] 请求限流，账号禁用并进入冷却", "email", email, "until", until.Format(time.RFC3339), "detail", detail)
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

// RecoverExpiredCooldowns 扫描全部账号，将限流冷却到期的账号恢复为 active。
func RecoverExpiredCooldowns() int {
	n := 0
	for _, account := range repository.LoadAccounts() {
		if account.IsCooldownExpired() {
			if repository.UpdateAccount(account.Email, func(a *repository.Account) {
				if a.IsCooldownExpired() {
					a.Status = repository.StatusActive
					a.DisabledUntil = nil
					a.DisableReason = ""
				}
			}) {
				slog.Info("[账号] 限流冷却结束，自动恢复启用", "email", account.Email)
				n++
			}
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
	return &PublicAccount{
		Email:         account.Email,
		OrgUUID:       account.OrgUUID,
		Status:        account.Status,
		DisabledUntil: account.DisabledUntil,
		DisableReason: account.DisableReason,
		CreatedAt:     account.CreatedAt,
		UpdatedAt:     account.UpdatedAt,
		HasSession:    SessionKey(account) != "",
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
		a.Email, a.OrgUUID, a.Status = info.Email, info.OrgUUID, "active"
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
