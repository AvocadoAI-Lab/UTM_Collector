package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "pico-utm-agent"
	displayName = "Pico-UTM Syslog Agent"
)

func enableUTF8Console() { _ = windows.SetConsoleOutputCP(65001) }

func isWindowsService() bool {
	ok, _ := svc.IsWindowsService()
	return ok
}

func runService(run func(ctx context.Context) error) error {
	return svc.Run(serviceName, &serviceHandler{run: run})
}

type serviceHandler struct {
	run func(ctx context.Context) error
}

func (h *serviceHandler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			if err != nil {
				return true, 1 // 非正常結束，交由 SCM 的 recovery 設定重啟
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

func installService(exe, configPath, _ string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("無法連線到服務管理員（需要系統管理員權限）：%w", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return errors.New("服務已存在，請先執行 agent uninstall")
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: displayName,
		Description: "接收 Pico-UTM Syslog 事件並轉送至遠端接收端",
		StartType:   mgr.StartAutomatic,
	}, "run", "--config", configPath)
	if err != nil {
		return fmt.Errorf("建立服務失敗：%w", err)
	}
	defer s.Close()
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 60 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 86400); err != nil {
		_ = s.Delete()
		return fmt.Errorf("設定失敗自動重啟失敗：%w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		_ = s.Delete()
		return fmt.Errorf("設定失敗自動重啟失敗：%w", err)
	}
	fmt.Printf("已建立 Windows 服務：%s（顯示名稱：%s）\n", serviceName, displayName)
	fmt.Printf("執行命令：\"%s\" run --config \"%s\"\n", exe, configPath)
	fmt.Println("執行帳號：LocalSystem")
	fmt.Println("啟動類型：自動（開機啟動）")
	fmt.Println("失敗處理：60 秒後自動重啟")
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("無法連線到服務管理員（需要系統管理員權限）：%w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return errors.New("服務不存在")
	}
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		fmt.Println("停止服務…")
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			s.Close()
			return fmt.Errorf("停止服務失敗：%w", err)
		}
		stopped := waitUntil(30*time.Second, func() bool {
			st, err := s.Query()
			return err != nil || st.State == svc.Stopped
		})
		if !stopped {
			s.Close()
			return errors.New("服務未能在 30 秒內停止")
		}
		fmt.Println("服務已停止")
	}
	err = s.Delete()
	s.Close()
	if err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("刪除服務失敗：%w", err)
	}
	// 若仍有程式持有服務 handle（例如開著 services.msc），服務只會被標記為待刪除
	removed := waitUntil(30*time.Second, func() bool {
		s2, err := m.OpenService(serviceName)
		if err != nil {
			return true
		}
		s2.Close()
		return false
	})
	if !removed {
		return errors.New("服務已標記為待刪除，但 30 秒內未完成移除；請關閉「服務」管理視窗（services.msc）後再試")
	}
	fmt.Printf("已刪除服務註冊：%s\n", serviceName)
	return nil
}

func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	return true
}
