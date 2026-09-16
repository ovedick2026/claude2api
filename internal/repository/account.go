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
