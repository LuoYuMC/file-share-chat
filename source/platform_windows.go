package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func defaultHostsPath() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "drivers", "etc", "hosts")
}

// relaunchAsAdmin 通过 ShellExecute 的 runas 动词，以管理员身份重新启动本程序（会弹出 UAC）
func relaunchAsAdmin(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exeDir := filepath.Dir(exe)
	params := append([]string{}, args...)
	params = append(params, "-elevated")

	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString(exe)
	dir, _ := syscall.UTF16PtrFromString(exeDir)
	paramStr, _ := syscall.UTF16PtrFromString(joinArgs(params))

	shell32 := syscall.NewLazyDLL("shell32.dll")
	shellExecute := shell32.NewProc("ShellExecuteW")
	ret, _, _ := shellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(paramStr)),
		uintptr(unsafe.Pointer(dir)),
		1, // SW_SHOWNORMAL
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecute 返回代码 %d（若取消了 UAC 提示，请重新运行并点击“是”）", ret)
	}
	return nil
}

func joinArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"") {
			a = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
		}
		quoted = append(quoted, a)
	}
	return strings.Join(quoted, " ")
}

func hiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// flushDNS 清除 Windows DNS 缓存，使 hosts 的修改尽快生效
func flushDNS() {
	_ = hiddenCmd("ipconfig", "/flushdns").Run()
}

// setupFirewall 添加入站防火墙规则（每个监听端口一条），允许局域网其他设备访问（需要管理员权限，失败不影响本机使用）
func setupFirewall(port int) {
	name := "FileShareChat-" + strconv.Itoa(port)
	_ = hiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule", "name=FileShareChat").Run() // 旧版本的规则
	_ = hiddenCmd("netsh", "advfirewall", "firewall", "delete", "rule", "name="+name).Run()
	err := hiddenCmd("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+name, "dir=in", "action=allow", "protocol=TCP",
		"localport="+strconv.Itoa(port), "profile=any").Run()
	if err != nil {
		fmt.Println("[提示] 添加防火墙规则失败（不影响本机访问）：", err)
	}
}

func openBrowser(url string) {
	_ = hiddenCmd("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// pause 双击运行时窗口会在结束后立即关闭，这里等待回车，方便看清输出
func pause() {
	fmt.Print("\n按回车键关闭窗口……")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// isElevated 判断当前进程是否以管理员（已提升）身份运行
func isElevated() bool {
	advapi32 := syscall.NewLazyDLL("advapi32.dll")
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getCurrentProcess := kernel32.NewProc("GetCurrentProcess")
	openProcessToken := advapi32.NewProc("OpenProcessToken")
	getTokenInformation := advapi32.NewProc("GetTokenInformation")

	proc, _, _ := getCurrentProcess.Call()
	var token syscall.Handle
	const tokenQuery = 0x0008
	if r, _, _ := openProcessToken.Call(proc, tokenQuery, uintptr(unsafe.Pointer(&token))); r == 0 {
		return false
	}
	defer syscall.CloseHandle(token)

	const tokenElevation = 20 // TOKEN_INFORMATION_CLASS: TokenElevation
	var elevation, size uint32
	r, _, _ := getTokenInformation.Call(uintptr(token), tokenElevation,
		uintptr(unsafe.Pointer(&elevation)), unsafe.Sizeof(elevation), uintptr(unsafe.Pointer(&size)))
	return r != 0 && elevation != 0
}
