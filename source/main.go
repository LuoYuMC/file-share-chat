// 文件分享 & 聊天 —— 单文件 Windows 程序（Go 标准库实现，无需安装任何运行环境）
//
// 功能：
//  1. 聊天：大群聊天（支持 @ 提醒）与一对一私聊；所有登录用户可见
//  2. 文件预览：登录后可在页面中预览 files\ 目录下的图片、视频、音频和文本（不提供下载）
//  3. 域名解析：管理员维护一个域名列表（可为每个域名指定目标 IP，默认 127.0.0.1），
//     列表中的域名会写入 hosts；通过这些域名访问时返回网页
//  4. 公告：主管理员与副管理员可发布、删除公告
//  5. 账号：注册（默认禁言，待管理员处理）、登录、修改密码；主管理员账号 admin（默认密码 admin123）
//     可设置副管理员、禁言用户和别名
//
// 页面 static/index.html 通过 go:embed 编译进 exe。
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

//go:embed static/index.html
var indexHTML []byte

type config struct {
	port      int
	host      string
	baseDir   string // 数据目录：files/、data/ 都在它下面（默认 exe 所在目录）
	hostsPath string
	noBrowser bool
	elevated  bool // 内部使用：标记本进程是已提权后重新启动的
}

func main() {
	cfg, err := parseConfig()
	if err != nil {
		fail(err)
	}
	if err := run(cfg); err != nil {
		fail(err)
	}
	pause()
}

func fail(err error) {
	fmt.Println()
	fmt.Println("[错误]", err)
	pause()
	os.Exit(1)
}

func parseConfig() (*config, error) {
	cfg := &config{}
	flag.IntVar(&cfg.port, "port", 0, "聊天与文件服务端口（不指定时使用管理页面设置的端口，未设置时为 80）")
	flag.StringVar(&cfg.host, "host", "0.0.0.0", "监听地址（0.0.0.0 表示允许局域网访问）")
	flag.StringVar(&cfg.baseDir, "dir", "", "数据目录（默认为程序所在目录）")
	flag.StringVar(&cfg.hostsPath, "hosts", "", "hosts 文件路径（默认为系统 hosts）")
	flag.BoolVar(&cfg.noBrowser, "no-browser", false, "启动后不自动打开浏览器")
	flag.BoolVar(&cfg.elevated, "elevated", false, "内部使用")
	flag.Parse()

	if cfg.baseDir == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("无法获取程序路径：%w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		cfg.baseDir = filepath.Dir(exe)
	}
	abs, err := filepath.Abs(cfg.baseDir)
	if err != nil {
		return nil, err
	}
	cfg.baseDir = abs
	if cfg.hostsPath == "" {
		cfg.hostsPath = defaultHostsPath()
	}
	return cfg, nil
}

func run(cfg *config) error {
	p := newPathSet(cfg.baseDir)
	if err := os.MkdirAll(p.files, 0755); err != nil {
		return fmt.Errorf("无法创建 files 目录：%w", err)
	}
	if err := os.MkdirAll(p.data, 0755); err != nil {
		return fmt.Errorf("无法创建 data 目录：%w", err)
	}

	// 修改 hosts 需要管理员权限：没有写权限时，以管理员身份重新启动自己
	if !cfg.elevated && !isElevated() && needsElevation(cfg.hostsPath) {
		fmt.Println("修改 hosts 文件需要管理员权限，正在请求提升权限……")
		fmt.Println("请在弹出的 UAC 窗口中点击“是”。")
		if err := relaunchAsAdmin(os.Args[1:]); err != nil {
			return fmt.Errorf("提升权限失败：%w", err)
		}
		fmt.Println("已在管理员窗口中启动，本窗口可以关闭。")
		return nil
	}

	store, err := openStore(p.db)
	if err != nil {
		return err
	}
	if err := ensureAdmin(store); err != nil {
		return err
	}
	srv := newServer(cfg, p, store)

	// 端口：命令行 -port 优先（便于临时调试）；否则使用管理页面保存的端口（未设置时为 80）
	port := cfg.port
	if port == 0 {
		port = store.ServerPort()
	}

	// 先抢占端口：若端口已被占用（例如 IIS 或已有一个实例在运行），直接退出，绝不能改动 hosts
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("无法监听端口 %d：%w\n       请检查是否已有程序占用该端口，或之前未退出的本程序\n       （可在命令行使用 -port 指定其他端口，启动后到管理页面修改）", port, err)
	}

	// 启动时按“域名列表”重写 hosts（同时清理上次遗留的记录，例如程序被强制关闭时）
	if hostsErr, _ := srv.updateHosts(nil); hostsErr != nil {
		fmt.Println("[警告]", hostsErr)
	} else {
		fmt.Printf("[启动] 已写入 hosts 域名列表（%d 个）\n", len(store.HostList()))
	}
	setupFirewall(port)
	srv.setPort(port)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	browserOpened := false
	for {
		printBanner(port, p.files, cfg.hostsPath)
		appSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
		errCh := make(chan error, 1)
		go func() { errCh <- appSrv.Serve(ln) }()

		if !browserOpened && !cfg.noBrowser {
			browserOpened = true
			url := siteURL("localhost", port)
			go func() {
				time.Sleep(time.Second)
				openBrowser(url)
			}()
		}

		select {
		case <-sigs:
			fmt.Println("\n正在停止服务……")
			shutdownHTTP(appSrv)
			<-errCh
			return restoreHosts(cfg, p)
		case err := <-errCh:
			_ = applyHosts(cfg.hostsPath, p.hostsBackup, nil)
			if err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("服务异常退出：%w", err)
			}
			return nil
		case req := <-srv.restartCh:
			// 管理页面保存了新端口：新端口已在 req.ln 上预先绑定，这里切换过去（hosts 不变，无需改动）
			fmt.Printf("\n[端口] 正在从端口 %d 切换到端口 %d……\n", port, req.port)
			shutdownHTTP(appSrv)
			<-errCh
			removeFirewall(port)
			ln, port = req.ln, req.port
			setupFirewall(port)
			srv.setPort(port)
			srv.finishRestart()
		}
	}
}

// shutdownHTTP 优雅关闭 HTTP 服务（最多等待 3 秒）
func shutdownHTTP(appSrv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = appSrv.Shutdown(ctx)
}

// restoreHosts 退出时恢复 hosts 文件
func restoreHosts(cfg *config, p pathSet) error {
	if err := applyHosts(cfg.hostsPath, p.hostsBackup, nil); err != nil {
		return fmt.Errorf("恢复 hosts 失败，请手动删除含 \"# [file-share-chat]\" 标记的行：%w", err)
	}
	fmt.Println("[退出] 已恢复 hosts 文件，服务已停止。")
	return nil
}

func printBanner(port int, filesDir, hostsPath string) {
	fmt.Println("==============================================================")
	fmt.Println(" 文件分享 + 聊天 已启动")
	fmt.Printf(" 聊天与文件：%s（本机访问）\n", siteURL("localhost", port))
	fmt.Printf(" 文件目录：%s\n", filesDir)
	fmt.Printf(" hosts   ：%s\n", hostsPath)
	fmt.Println(" 停止服务：按 Ctrl+C（会自动恢复 hosts）")
	fmt.Println("==============================================================")
}

// ensureAdmin 确保存在管理员账号（用户名 admin，默认密码 admin123）
func ensureAdmin(store *Store) error {
	if store.HasAdmin() {
		return nil
	}
	hash, err := hashPassword(defaultAdminPass)
	if err != nil {
		return err
	}
	renamed, err := store.CreateAdmin(hash)
	if err != nil {
		return fmt.Errorf("创建管理员账号失败：%w", err)
	}
	if renamed != "" {
		fmt.Printf("[提示] 原有用户 %s；用户名 admin 现已保留给管理员\n", renamed)
	}
	fmt.Println("[启动] 已创建管理员账号 admin（默认密码 admin123，请登录后在“账号与密码”中修改）")
	return nil
}
