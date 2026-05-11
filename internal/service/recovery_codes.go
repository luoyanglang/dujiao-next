package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// recoveryCodeBcryptCost 恢复码的 bcrypt cost（与 admin/user TOTP 保持一致）
const recoveryCodeBcryptCost = 10

// recoveryCodeEntry 恢复码持久化结构
type recoveryCodeEntry struct {
	Hash   string     `json:"hash"`
	UsedAt *time.Time `json:"used_at,omitempty"`
}

// generateRecoveryCodesPair 生成 n 个明文恢复码与对应的 JSON 序列化（hash 入库）。
// 明文格式 xxxx-xxxx，5 字节随机 → 10 hex 字符 → 拆成两段。
func generateRecoveryCodesPair(n int) (plaintext []string, codesJSON string, err error) {
	plaintext = make([]string, 0, n)
	entries := make([]recoveryCodeEntry, 0, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 5)
		if _, err := rand.Read(raw); err != nil {
			return nil, "", err
		}
		hexStr := hex.EncodeToString(raw)
		formatted := hexStr[:4] + "-" + hexStr[4:]
		hash, err := bcrypt.GenerateFromPassword([]byte(formatted), recoveryCodeBcryptCost)
		if err != nil {
			return nil, "", err
		}
		plaintext = append(plaintext, formatted)
		entries = append(entries, recoveryCodeEntry{Hash: string(hash)})
	}
	js, err := json.Marshal(entries)
	if err != nil {
		return nil, "", err
	}
	return plaintext, string(js), nil
}

// decodeRecoveryCodesJSON 解析数据库存储的恢复码 JSON。
func decodeRecoveryCodesJSON(s string) ([]recoveryCodeEntry, error) {
	if s == "" {
		return []recoveryCodeEntry{}, nil
	}
	var out []recoveryCodeEntry
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// matchAndConsumeRecoveryCode 在 entries 中找到与 code 匹配且未被使用的项，
// 标记为已使用并返回更新后的 JSON。匹配失败返回 ErrTOTPRecoveryInvalid。
func matchAndConsumeRecoveryCode(entriesJSON, code string, now time.Time) (string, error) {
	code = strings.TrimSpace(strings.ToLower(code))
	if code == "" {
		return "", ErrTOTPRecoveryInvalid
	}
	entries, err := decodeRecoveryCodesJSON(entriesJSON)
	if err != nil {
		return "", fmt.Errorf("decode recovery: %w", err)
	}
	matched := -1
	for i, e := range entries {
		if e.UsedAt != nil {
			continue
		}
		if bcrypt.CompareHashAndPassword([]byte(e.Hash), []byte(code)) == nil {
			matched = i
			break
		}
	}
	if matched < 0 {
		return "", ErrTOTPRecoveryInvalid
	}
	t := now
	entries[matched].UsedAt = &t
	js, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(js), nil
}
