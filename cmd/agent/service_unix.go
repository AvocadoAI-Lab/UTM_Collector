//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	serviceName = "pico-utm-agent"
	unitPath    = "/etc/systemd/system/pico-utm-agent.service"
)

const unitTemplate = `[Unit]
Description=Pico-UTM Syslog Agent
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=%[3]s
Group=%[3]s
ExecStart=%[1]s run --config %[2]s
Restart=always
RestartSec=60

[Install]
WantedBy=multi-user.target
`

func enableUTF8Console() {}

func isWindowsService() bool { return false }

func runService(func(ctx context.Context) error) error {
	return errors.New("此平台不支援 Windows Service")
}

func installService(exe, configPath, user string) error {
	if os.Geteuid() != 0 {
		return errors.New("需要 root 權限")
	}
	if _, err := os.Stat(unitPath); err == nil {
		return fmt.Errorf("服務已存在（%s），請先執行 agent uninstall", unitPath)
	}
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(unitTemplate, exe, configPath, user)), 0o644); err != nil {
		return err
	}
	fmt.Printf("已寫入 systemd unit：%s\n", unitPath)
	fmt.Printf("執行命令：%s run --config %s\n", exe, configPath)
	fmt.Printf("執行帳號：%s\n", user)
	fmt.Println("重啟策略：Restart=always、RestartSec=60；After=network-online.target")
	if err := systemctl("daemon-reload"); err != nil {
		os.Remove(unitPath)
		return err
	}
	fmt.Println("已執行 systemctl daemon-reload")
	if err := systemctl("enable", serviceName); err != nil {
		_ = systemctl("disable", serviceName)
		os.Remove(unitPath)
		_ = systemctl("daemon-reload")
		return err
	}
	fmt.Printf("已設定開機自動啟動（systemctl enable %s）\n", serviceName)
	return nil
}

func uninstallService() error {
	if os.Geteuid() != 0 {
		return errors.New("需要 root 權限")
	}
	if _, err := os.Stat(unitPath); err != nil {
		return errors.New("服務不存在")
	}
	if err := systemctl("disable", "--now", serviceName); err != nil {
		return fmt.Errorf("停止服務失敗，保留 unit 以便重試：%w", err)
	} else {
		fmt.Printf("已停止並取消開機自動啟動（systemctl disable --now %s）\n", serviceName)
	}
	if err := os.Remove(unitPath); err != nil {
		return err
	}
	fmt.Printf("已刪除 systemd unit：%s\n", unitPath)
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	_ = systemctl("reset-failed", serviceName)
	fmt.Println("已執行 systemctl daemon-reload")
	return nil
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s 失敗：%v：%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
