package repository

import (
	"time"

	"gorm.io/gorm/clause"
)

// 账号状态常量。
const (
	StatusActive   = "active"
	StatusExpired  = "expired"
	StatusError    = "error"
	StatusCooldown = "cooldown" // 限流冷却中，DisabledUntil 到期后自动恢复 active
	StatusDisabled = "disabled" // 非限流报错被禁用，不自动恢复
)

// Account 是一条账号记录。
type Account struct {
	ID            uint              `json:"-" gorm:"primaryKey"`
	Email         string            `json:"email" gorm:"uniqueIndex;not null"`
	OrgUUID       string            `json:"org_uuid" gorm:"column:org_uuid"`
	Cookies       map[string]string `json:"-" gorm:"serializer:json"`
	Status        string            `json:"status,omitempty"`         // active/expired/error/cooldown/disabled
	DisabledUntil *time.Time        `json:"disabled_until"`           // 限流冷却截止时间（上游 resetsAt），nil 表示无冷却
	DisableReason string            `json:"disable_reason,omitempty"` // 自动禁用原因：限流为 "rate_limit"，其他为原始错误摘要
	Opus5Date     string            `json:"opus5_date"`               // claude-opus-5 当日额度计数日期（本地自然日 YYYY-MM-DD，0点重置）
	Opus5Count    int               `json:"opus5_count"`              // claude-opus-5 当日已成功调用次数
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// IsCooldownExpired 判断限流冷却是否已结束（用于自动恢复判断）。
func (a *Account) IsCooldownExpired() bool {
	return a.Status == StatusCooldown && a.DisabledUntil != nil && !a.DisabledUntil.After(time.Now())
}

// LoadAccounts 读取全部账号。
func LoadAccounts() []Account {
	var out []Account
	if err := db.Order("id ASC").Find(&out).Error; err != nil {
		return []Account{}
	}
	return out
}

// AccountByEmail 按邮箱取账号。
func AccountByEmail(email string) *Account {
	if email == "" {
		return nil
	}
	var a Account
	if db.Where("email = ?", email).First(&a).Error != nil {
		return nil
	}
	return &a
}

// UpsertAccount 写入或更新账号。
func UpsertAccount(a *Account) error {
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "email"}},
		UpdateAll: true,
	}).Create(a).Error
}

// UpdateAccount 修改指定账号。
func UpdateAccount(email string, mutate func(*Account)) bool {
	a := AccountByEmail(email)
	if a == nil {
		return false
	}
	mutate(a)
	return db.Save(a).Error == nil
}

// DeleteAccount 删除账号。
func DeleteAccount(email string) int {
	res := db.Where("email = ?", email).Delete(&Account{})
	if res.Error != nil {
		return 0
	}
	return int(res.RowsAffected)
}

// DeleteAccountsByStatus 按状态删除账号。
func DeleteAccountsByStatus(statuses []string) []string {
	removed := make([]string, 0)
	for _, a := range LoadAccounts() {
		for _, s := range statuses {
			if a.Status == s {
				if db.Where("email = ?", a.Email).Delete(&Account{}).Error == nil {
					removed = append(removed, a.Email)
				}
				break
			}
		}
	}
	return removed
}

// DeleteAccounts 批量删除指定邮箱的账号，返回成功删除的邮箱。
func DeleteAccounts(emails []string) []string {
	removed := make([]string, 0)
	for _, email := range emails {
		if email == "" {
			continue
		}
		if db.Where("email = ?", email).Delete(&Account{}).Error == nil {
			removed = append(removed, email)
		}
	}
	return removed
}

// Opus5DailyLimit claude-opus-5 每账号每日调用上限。
const Opus5DailyLimit = 3

// opus5Today 返回本地自然日日期串（YYYY-MM-DD，自然日0点重置）。
func opus5Today() string {
	return time.Now().Format("2006-01-02")
}

// Opus5QuotaExhausted 判断账号当日 claude-opus-5 额度是否已用完（记录日期非当日视为未使用）。
func (a *Account) Opus5QuotaExhausted() bool {
	if a == nil {
		return false
	}
	count := a.Opus5Count
	if a.Opus5Date != opus5Today() {
		count = 0
	}
	return count >= Opus5DailyLimit
}

// IncrOpus5Usage 记录一次 claude-opus-5 成功调用（自然日0点重置计数，随账号持久化）。
func IncrOpus5Usage(email string) {
	if email == "" {
		return
	}
	UpdateAccount(email, func(a *Account) {
		today := opus5Today()
		if a.Opus5Date != today {
			a.Opus5Date = today
			a.Opus5Count = 0
		}
		a.Opus5Count++
	})
}

// Opus5CountToday 返回账号当日已用 claude-opus-5 次数（记录日期非当日按0计）。
func Opus5CountToday(email string) int {
	a := AccountByEmail(email)
	if a == nil || a.Opus5Date != opus5Today() {
		return 0
	}
	return a.Opus5Count
}

// Opus5Remaining 返回账号当日 claude-opus-5 剩余可用次数（记录日期非当日视为未使用，按满额计）。
func (a *Account) Opus5Remaining() int {
	if a == nil {
		return 0
	}
	n := Opus5DailyLimit
	if a.Opus5Date == opus5Today() {
		n -= a.Opus5Count
	}
	if n < 0 {
		n = 0
	}
	return n
}
