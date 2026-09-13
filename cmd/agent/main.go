package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"pico-utm-agent/internal/agent"
)

const usageText = `pico-utm-agent — Pico-UTM Syslog 收集轉送 Agent

用法：
  agent run        [--config 路徑]   前景執行（服務模式呼叫此命令）
  agent test       [--config 路徑]   自我檢查，不修改任何狀態
  agent status     [--config 路徑]   顯示執行狀態與統計
  agent tail       [--config 路徑]   即時顯示收到的事件
  agent logs       [--config 路徑] [-n 行數]   顯示最新 log 檔的最後 N 行（預設 50）
  agent install    [--config 路徑] [--user 帳號]   安裝為系統服務
  agent uninstall                    移除服務
  agent version                      版本資訊

預設設定檔：%s
`

func main() {
	enableUTF8Console()
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usageText, agent.DefaultConfigPath())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var code int
	switch cmd {
	case "run":
		code = cmdRun(args)
	case "test":
		code = withConfig("test", args, func(ctx context.Context, path string) int {
			return agent.SelfTest(ctx, path, os.Stdout)
		})
	case "status":
		code = withConfig("status", args, func(_ context.Context, path string) int {
			return agent.PrintStatus(path, os.Stdout)
		})
	case "tail":
		code = withConfig("tail", args, cmdTail)
	case "logs":
		code = cmdLogs(args)
	case "install":
		code = cmdInstall(args)
	case "uninstall":
		code = cmdUninstall()
	case "version", "--version", "-v":
		fmt.Printf("pico-utm-agent %s (%s %s/%s)\n", agent.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		fmt.Printf(usageText, agent.DefaultConfigPath())
	default:
		fmt.Fprintf(os.Stderr, "未知的命令：%s\n\n", cmd)
		fmt.Fprintf(os.Stderr, usageText, agent.DefaultConfigPath())
		code = 2
	}
	os.Exit(code)
}

func withConfig(name string, args []string, fn func(ctx context.Context, path string) int) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := fs.String("config", agent.DefaultConfigPath(), "設定檔路徑")
	if fs.Parse(args) != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return fn(ctx, *configPath)
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", agent.DefaultConfigPath(), "設定檔路徑")
	if fs.Parse(args) != nil {
		return 2
	}
	if isWindowsService() {
		if err := runService(func(ctx context.Context) error { return runAgent(ctx, *configPath, nil) }); err != nil {
			return 1
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runAgent(ctx, *configPath, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func runAgent(ctx context.Context, configPath string, console io.Writer) error {
	cfg, err := agent.LoadConfig(configPath)
	if err != nil {
		return err
	}
	log, closer, err := agent.NewLogger(cfg, console)
	if err != nil {
		return fmt.Errorf("建立 log 失敗：%w", err)
	}
	defer closer.Close()
	log.Info("設定載入成功", "config", configPath, "summary", cfg.Summary())
	if wide, detail, err := agent.ConfigPermissionTooWide(configPath); err == nil && wide {
		log.Warn("設定檔權限過寬", "config", configPath, "detail", detail)
	}
	a, err := agent.New(cfg, agent.Options{Logger: log})
	if err != nil {
		log.Error("agent 初始化失敗", "error", err)
		return err
	}
	return a.Run(ctx)
}

func cmdTail(ctx context.Context, path string) int {
	cfg, err := agent.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("監看 %s 的新事件（Ctrl+C 結束）\n", cfg.Spool.Dir)
	if err := agent.Tail(ctx, cfg.Spool.Dir, os.Stdout, 500*time.Millisecond); err != nil {
		fmt.Fprintln(os.Stderr, "無法監看 spool：", err)
		return 1
	}
	return 0
}

func cmdLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	configPath := fs.String("config", agent.DefaultConfigPath(), "設定檔路徑")
	n := fs.Int("n", 50, "顯示最後 N 行")
	if fs.Parse(args) != nil {
		return 2
	}
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := agent.PrintLogTail(cfg.Logging.Dir, *n, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "無法讀取 log：", err)
		return 1
	}
	return 0
}

func cmdInstall(args []string) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	configPath := fs.String("config", agent.DefaultConfigPath(), "設定檔路徑")
	user := fs.String("user", "pico-utm-agent", "執行服務的帳號（僅 Linux）")
	if fs.Parse(args) != nil {
		return 2
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	cfgAbs, err2 := filepath.Abs(*configPath)
	if err != nil || err2 != nil {
		fmt.Fprintln(os.Stderr, "無法取得執行檔或設定檔的絕對路徑")
		return 1
	}
	if err := installService(exe, cfgAbs, *user); err != nil {
		fmt.Fprintln(os.Stderr, "安裝服務失敗：", err)
		return 1
	}
	fmt.Println("服務已註冊，並設定為開機自動啟動")
	return 0
}

func cmdUninstall() int {
	if err := uninstallService(); err != nil {
		fmt.Fprintln(os.Stderr, "移除服務失敗：", err)
		return 1
	}
	fmt.Println("服務已移除")
	return 0
}
