package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"otun-node-agent/internal/tunnel"
)

// obox-key：在 VPS 上管理「谁能进这台机器」。
//
// 设计文档 OBOX_TUNNEL_OWNER_KEY_DESIGN §3.3。
//
// ⚠️ 这是增删钥匙的**唯一**入口，而且只能在本机执行。
// 后端没有任何接口能改这个列表 —— 那是本方案的核心不变量：
// 一旦做成「App 调后端 → 后端下指令加钥匙」，后门就回来了。
//
// 三条路等价，凡是能在这台机器上拿到 root 的人都能用：
//   ① 云厂商网页控制台
//   ② 22 端口 SSH
//   ③ 经 OBox 隧道进来的终端
//
// 以 agent 二进制的子命令实现，install.sh 建一个软链到
// /usr/local/bin/obox-key。这样不必单独构建、单独校验一个二进制。

const oboxKeyUsage = `obox-key —— 管理允许登录本机 OBox 终端的设备

用法：
  obox-key list                       列出已授权的设备
  obox-key add  "ssh-ed25519 AAAA… 设备名"   添加一台设备
  obox-key rm   SHA256:…              移除一台设备（不能删到一把不剩）

说明：
  这个列表只存在本机，后端改不了，也拿不到。
  想让新手机能进，就在这里加它的公钥；想收回，就删掉。
`

// isOboxKeyInvocation 判断这次是不是在当 obox-key 用。
//
// 两种调用方式都支持：
//
//	obox-key list          （软链，argv[0] 以 obox-key 结尾）
//	agent obox-key list    （直接带子命令，便于调试与测试）
func isOboxKeyInvocation() bool {
	if strings.HasSuffix(filepath.Base(os.Args[0]), "obox-key") {
		return true
	}
	return len(os.Args) > 1 && os.Args[1] == "obox-key"
}

// oboxKeyArgs 取出子命令之后的参数。
func oboxKeyArgs() []string {
	if strings.HasSuffix(filepath.Base(os.Args[0]), "obox-key") {
		return os.Args[1:]
	}
	return os.Args[2:]
}

// runOboxKey 处理 obox-key 子命令。返回退出码。
func runOboxKey(args []string) int {
	if len(args) == 0 {
		fmt.Print(oboxKeyUsage)
		return 2
	}

	store := tunnel.NewAuthKeyStore(oboxKeyDataDir())

	switch args[0] {
	case "list", "ls":
		return oboxKeyList(store)

	case "add":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "用法：obox-key add \"ssh-ed25519 AAAA… 设备名\"")
			return 2
		}
		return oboxKeyAdd(store, strings.Join(args[1:], " "))

	case "rm", "remove", "del":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "用法：obox-key rm SHA256:…")
			return 2
		}
		return oboxKeyRemove(store, args[1])

	case "-h", "--help", "help":
		fmt.Print(oboxKeyUsage)
		return 0

	default:
		fmt.Fprintf(os.Stderr, "未知子命令：%s\n\n%s", args[0], oboxKeyUsage)
		return 2
	}
}

// oboxKeyDataDir 定位授权列表所在目录。
//
// agent 由 systemd 以 WorkingDirectory=/opt/otun-agent 启动，用的是
// 相对路径 ./data。但 obox-key 是用户在任意目录敲的，不能跟着 cwd 走 ——
// 那样会在用户当前目录造出一个空列表，看起来像「钥匙全没了」。
func oboxKeyDataDir() string {
	if d := os.Getenv("OTUN_DATA_DIR"); d != "" {
		return d
	}
	return "/opt/otun-agent/data"
}

func oboxKeyList(store *tunnel.AuthKeyStore) int {
	keys, err := store.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取失败：%v\n", err)
		return 1
	}
	if len(keys) == 0 {
		fmt.Println("还没有授权任何设备 —— 当前谁都无法从 App 打开这台机器的终端。")
		fmt.Println("要授权，请从 App 重新生成安装命令并执行，或在这里 obox-key add。")
		return 0
	}
	fmt.Printf("已授权 %d 台设备：\n\n", len(keys))
	for _, k := range keys {
		name := k.Name
		if name == "" {
			name = "(未命名)"
		}
		fmt.Printf("  %s\n    %s\n    添加于 %s（来源 %s）\n\n",
			name, k.FP, k.AddedAt, k.AddedBy)
	}
	return 0
}

func oboxKeyAdd(store *tunnel.AuthKeyStore, line string) int {
	k, err := store.Add(line, oboxKeyAddedBy())
	if err != nil {
		fmt.Fprintf(os.Stderr, "添加失败：%v\n", err)
		return 1
	}
	fmt.Printf("已授权 %s\n  %s\n", displayName(k.Name), k.FP)
	return 0
}

func oboxKeyRemove(store *tunnel.AuthKeyStore, fp string) int {
	if err := store.Remove(fp); err != nil {
		fmt.Fprintf(os.Stderr, "移除失败：%v\n", err)
		return 1
	}
	fmt.Printf("已移除 %s\n", fp)
	return 0
}

// oboxKeyAddedBy 记录这次操作从哪来，仅用于事后追溯。
//
// 经隧道进来的会话带 OBOX_SESSION_FP（认证用的那把钥匙的指纹）；
// 云控制台或 22 端口进来的没有，记 local。
//
// ⚠️ 这不是门槛 —— root 本来就能直接改文件，加门槛只是自欺。
func oboxKeyAddedBy() string {
	if fp := os.Getenv("OBOX_SESSION_FP"); fp != "" {
		return "session:" + fp
	}
	return "local"
}

func displayName(n string) string {
	if n == "" {
		return "(未命名设备)"
	}
	return n
}
