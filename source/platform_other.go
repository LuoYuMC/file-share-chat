//go:build !windows

// 非 Windows 系统的兼容实现（仅用于在 Linux 上做功能测试；正式使用的是 Windows 版 exe）

package main

import (
	"errors"
	"os"
	"os/exec"
)

func defaultHostsPath() string { return "/etc/hosts" }

func relaunchAsAdmin(args []string) error {
	return errors.New("自动提权仅支持 Windows，请以 root 身份运行")
}

func flushDNS() {}

func setupFirewall(port int) {}

func removeFirewall(port int) {}

func openBrowser(url string) {
	_ = exec.Command("xdg-open", url).Start()
}

func pause() {}

func isElevated() bool { return os.Geteuid() == 0 }
