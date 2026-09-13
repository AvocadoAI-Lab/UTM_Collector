package agent

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const firewallRuleName = "Pico-UTM Syslog"

func DiskFree(dir string) (int64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(free), nil
}

// ConfigPermissionTooWide 檢查 DACL 是否允許 Everyone / Authenticated Users / Users 讀取。
func ConfigPermissionTooWide(path string) (bool, string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, "", err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, "", err
	}
	if dacl == nil {
		return true, "檔案沒有 DACL（任何人皆可存取）", nil
	}
	wide := map[string]string{"S-1-1-0": "Everyone", "S-1-5-11": "Authenticated Users", "S-1-5-32-545": "Users"}
	const readBits = 0x1 | 0x80000000 | 0x10000000 // FILE_READ_DATA | GENERIC_READ | GENERIC_ALL
	var found []string
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || uint32(ace.Mask)&readBits == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if name, ok := wide[sid.String()]; ok {
			found = append(found, name)
		}
	}
	if len(found) > 0 {
		return true, strings.Join(found, "、") + " 可讀取", nil
	}
	return false, "", nil
}

func permissionAdvice(path string) []string {
	return []string{"建議：以系統管理員執行", fmt.Sprintf(`icacls "%s" /inheritance:r /grant:r *S-1-5-18:F *S-1-5-32-544:F`, path)}
}

func checkFirewall(p *checkPrinter, port int) {
	cmd := exec.Command("netsh")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`netsh advfirewall firewall show rule name="%s"`, firewallRuleName)}
	if err := cmd.Run(); err == nil {
		p.ok("防火牆規則「%s」存在", firewallRuleName)
		return
	}
	p.fail("防火牆規則不存在",
		"建議：以系統管理員執行",
		fmt.Sprintf(`netsh advfirewall firewall add rule name="%s" dir=in action=allow protocol=UDP localport=%d`, firewallRuleName, port))
}
