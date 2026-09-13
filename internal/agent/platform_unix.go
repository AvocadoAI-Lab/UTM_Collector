//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
)

func DiskFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func ConfigPermissionTooWide(path string) (bool, string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, "", err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return true, fmt.Sprintf("權限為 %04o（應為 0600）", perm), nil
	}
	return false, "", nil
}

func permissionAdvice(path string) []string {
	return []string{"建議：以 root 執行", "chmod 600 " + path}
}

func checkFirewall(p *checkPrinter, port int) {
	if path, err := exec.LookPath("ufw"); err == nil {
		out, err := exec.Command(path, "status").CombinedOutput()
		if err != nil {
			p.fail("無法查詢 ufw 狀態："+strings.TrimSpace(string(out)), "建議：以 root 執行 agent test")
			return
		}
		s := string(out)
		switch {
		case !strings.Contains(s, "Status: active"):
			p.ok("ufw 未啟用")
		case strings.Contains(s, fmt.Sprintf("%d/udp", port)):
			p.ok("防火牆（ufw）已允許 UDP %d", port)
		default:
			p.fail("防火牆規則不存在", "建議：以 root 執行", fmt.Sprintf("ufw allow %d/udp", port))
		}
		return
	}
	p.ok("未偵測到 ufw（若使用其他防火牆，請確認已允許 UDP %d）", port)
}
