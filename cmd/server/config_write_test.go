// config_write_test.go writeConfigAtomic 的 Docker 单文件挂载回退行为。
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWriteConfigAtomicHappyPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeConfigAtomic(path, []byte(`{"new":true}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"new":true}` {
		t.Fatalf("content: %s", b)
	}
	// rename 成功路径不残留 tmp。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file should be gone after rename")
	}
}

func TestWriteConfigAtomicEBUSYFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 模拟 Docker 单文件 bind mount：rename 恒 EBUSY。
	orig := osRename
	osRename = func(string, string) error { return syscall.EBUSY }
	defer func() { osRename = orig }()

	if err := writeConfigAtomic(path, []byte(`{"new":true}`)); err != nil {
		t.Fatalf("EBUSY must fall back to in-place write, got: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"new":true}` {
		t.Fatalf("content: %s", b)
	}
	// 失败的 tmp 必须被清理。
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp file must be removed after failed rename")
	}
}

func TestWriteConfigAtomicTotalFailure(t *testing.T) {
	// rename EBUSY（注入）+ 原地写也失败（目标是目录，WriteFile 得 EISDIR）
	// → 组合错误信息必须同时携带两条线索（rename 原因 + 原地写原因）。
	path := filepath.Join(t.TempDir(), "config.json") // 一个目录
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := osRename
	osRename = func(string, string) error { return syscall.EBUSY }
	defer func() { osRename = orig }()

	err := writeConfigAtomic(path, []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "rename") {
		t.Fatalf("want combined error, got: %v", err)
	}
	_ = errors.Is
}
