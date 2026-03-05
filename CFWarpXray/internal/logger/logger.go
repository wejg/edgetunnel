// Package logger 提供统一格式的应用日志：时间戳、级别、组件、消息，
// 便于生产环境排查与日志采集（如 ELK）解析。
package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	LevelInfo  = "INFO"
	LevelWarn  = "WARN"
	LevelError = "ERROR"
)

const timeFormat = "2006-01-02 15:04:05"

var mu sync.Mutex

// Line 生成单行日志：[时间] [级别] [组件] 消息
func Line(level, component, msg string) string {
	return fmt.Sprintf("[%s] [%s] [%s] %s",
		time.Now().Format(timeFormat), level, component, msg)
}

// Stdout 向标准输出打印一行日志，带时间、级别、组件
func Stdout(level, component, msg string) {
	mu.Lock()
	defer mu.Unlock()
	fmt.Println(Line(level, component, msg))
}

// Stderr 向标准错误打印一行日志
func Stderr(level, component, msg string) {
	mu.Lock()
	defer mu.Unlock()
	_, _ = fmt.Fprintln(os.Stderr, Line(level, component, msg))
}

// ToFile 将一行日志追加写入指定文件，并同时打印到标准输出
func ToFile(logPath, level, component, msg string) {
	line := Line(level, component, msg)
	Stdout(level, component, msg)
	mu.Lock()
	defer mu.Unlock()
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}

// ToFileOnly 仅写入文件，不输出到 stdout（用于监控日志等避免刷屏）
func ToFileOnly(logPath, level, component, msg string) {
	line := Line(level, component, msg)
	mu.Lock()
	defer mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(logPath), 0755)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}
