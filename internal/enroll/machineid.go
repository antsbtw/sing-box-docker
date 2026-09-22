package enroll

// machine_id 的计算与持久化。接线契约 §3。
//
// 后端按 (user_id, machine_id) 去重：同一台机器重跑安装命令应当更新原节点、
// 轮换 secret，而不是新建一条。所以这个值必须在重装后保持不变 ——
// 这正是不能把"首次注册时间"掺进去的原因（那会让每次重装都算新机器，
// 与去重目标相反）。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	pathMachineID  = "/etc/machine-id"
	pathProductUUID = "/sys/class/dmi/id/product_uuid"
	fallbackFile   = "machine_id" // 相对 dataDir
)

// MachineID 计算本机指纹，返回 64 位小写 hex。
//
// 优先级（契约 §3）：
//  1. sha256(/etc/machine-id | /sys/class/dmi/id/product_uuid)
//     product_uuid 是云厂商给的实例 UUID，克隆镜像也不会重复 ——
//     这正是它存在的意义：/etc/machine-id 在克隆出来的镜像里可能相同。
//  2. product_uuid 读不到（容器 / 无 DMI）→ 用第一块物理网卡 MAC 代替
//  3. 两者都读不到 → 随机 32 字节，持久化到 dataDir，此后一直读它
func MachineID(dataDir string) (string, error) {
	mid := readTrimmed(pathMachineID)
	uuid := readTrimmed(pathProductUUID)

	if uuid == "" {
		uuid = firstPhysicalMAC()
	}

	// 两个来源都拿不到：退到持久化的随机值。
	// 必须持久化，否则每次重启都变成"新机器"。
	if mid == "" && uuid == "" {
		return persistedRandomID(dataDir)
	}

	sum := sha256.Sum256([]byte(mid + "|" + uuid))
	return hex.EncodeToString(sum[:]), nil
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// firstPhysicalMAC 返回第一块物理网卡的 MAC（小写，冒号分隔）。
//
// 排除 lo / docker* / veth* / br-* —— 这些是虚拟接口，
// docker 网桥的 MAC 在不同机器上可能相同，veth 则每次重启都变。
func firstPhysicalMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	// 按名字排序，保证多网卡时每次取到同一块
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].Name < ifaces[j].Name })

	for _, iface := range ifaces {
		name := iface.Name
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") {
			continue
		}
		if mac := iface.HardwareAddr.String(); mac != "" {
			return strings.ToLower(mac)
		}
	}
	return ""
}

func persistedRandomID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, fallbackFile)

	if existing := readTrimmed(path); len(existing) == 64 {
		return existing, nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("persist machine id: %w", err)
	}
	return id, nil
}
