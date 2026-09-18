package repository

import "encoding/json"

// APILog 是 2api 调用日志。
type APILog struct {
	ID           int64           `json:"id" gorm:"primaryKey"`
	CreatedAt    string          `json:"created_at"`
	Endpoint     string          `json:"endpoint"`
	Model        string          `json:"model"`
	Account      string          `json:"account"`
	Stream       bool            `json:"stream"`
	Success      bool            `json:"success"`
	StatusCode   int             `json:"status_code"`
	InputTokens  int             `json:"input_tokens"`
	OutputTokens int             `json:"output_tokens"`
	DurationMs   int64           `json:"duration_ms"`
	FirstTokenMs int64           `json:"first_token_ms"`
	TPS          float64         `json:"tps"`
	Error        string          `json:"error"`
	Request      json.RawMessage `json:"request" gorm:"type:text;serializer:json"`
	Response     string          `json:"response" gorm:"type:text"`
}

func GetAPILog(id int64) (APILog, bool) {
	var out APILog
	if db.First(&out, id).Error != nil {
		return APILog{}, false
	}
	return out, true
}

// InsertAPILog 写入调用日志。
func InsertAPILog(l APILog) {
	if db == nil {
		return
	}
	if l.CreatedAt == "" {
		l.CreatedAt = nowUTC()
	}
	_ = db.Create(&l).Error
}

// ListAPILogs 返回调用日志。
func ListAPILogs(limit, offset int) []APILog {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var out []APILog
	if err := db.Omit("Request", "Response").Order("id DESC").Limit(limit).Offset(offset).Find(&out).Error; err != nil {
		return []APILog{}
	}
	return out
}

// CountAPILogs 返回日志数。
func CountAPILogs() int {
	var n int64
	if err := db.Model(&APILog{}).Count(&n).Error; err != nil {
		return 0
	}
	return int(n)
}

func DeleteAPILogs(ids []int64) int64 {
	return db.Delete(&APILog{}, ids).RowsAffected
}

func TrimAPILogs(keep int) int64 {
	if keep == 0 {
		return db.Where("id > 0").Delete(&APILog{}).RowsAffected
	}
	var cutoff APILog
	if db.Select("id").Order("id DESC").Offset(keep).Take(&cutoff).Error != nil {
		return 0
	}
	return db.Where("id <= ?", cutoff.ID).Delete(&APILog{}).RowsAffected
}

// SumTokenUsage 汇总指定账号自 sinceUTC（含）以来成功请求的输入/输出 token 用量。
// 数据来源为本地 APILog 调用日志（仅统计成功请求），created_at 为 UTC ISO 格式字符串，
// 字典序与时间序一致，可直接做字符串比较，供 5 小时/7 天额度显示使用。
func SumTokenUsage(email, sinceUTC string) (input, output int) {
	if db == nil || email == "" {
		return 0, 0
	}
	var row struct {
		In  int64 `gorm:"column:in_tok"`
		Out int64 `gorm:"column:out_tok"`
	}
	err := db.Model(&APILog{}).
		Select("COALESCE(SUM(input_tokens),0) AS in_tok, COALESCE(SUM(output_tokens),0) AS out_tok").
		Where("account = ? AND success = ? AND created_at >= ?", email, true, sinceUTC).
		Scan(&row).Error
	if err != nil {
		return 0, 0
	}
	return int(row.In), int(row.Out)
}
