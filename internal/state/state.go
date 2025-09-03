// 文件路径: internal/state/state.go
package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// stateDir 定义了存放状态文件的目录名。
// 将其放在项目根目录下，方便查看和管理。
const stateDir = "states"

// LoadLastUID 为指定邮箱加载上一次保存的 UID。
func LoadLastUID(email string) (uint32, error) {
	// 确保状态目录存在
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return 0, fmt.Errorf("无法创建状态目录 '%s': %w", stateDir, err)
	}

	fileName := filepath.Join(stateDir, fmt.Sprintf("state_%s.txt", email))
	data, err := os.ReadFile(fileName)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // 文件不存在，说明是第一次运行，返回 UID 0
		}
		return 0, fmt.Errorf("无法读取状态文件 '%s': %w", fileName, err)
	}

	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0, nil // 文件为空
	}

	uid, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("解析UID失败 '%s': %w", s, err)
	}

	return uint32(uid), nil
}

// SaveLastUID 为指定邮箱保存最新的 UID。
func SaveLastUID(email string, uid uint32) error {
	// 确保状态目录存在
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return fmt.Errorf("无法创建状态目录 '%s': %w", stateDir, err)
	}

	fileName := filepath.Join(stateDir, fmt.Sprintf("state_%s.txt", email))
	data := []byte(strconv.FormatUint(uint64(uid), 10))

	// 将 UID 写入文件，覆盖旧内容
	return os.WriteFile(fileName, data, 0644)
}
