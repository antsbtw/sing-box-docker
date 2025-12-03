package stats

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// SystemLoad 系统负载信息
type SystemLoad struct {
	CPUPercent    float64
	MemoryPercent float64
}

// GetSystemLoad 获取系统负载（简化版，Linux 专用）
func GetSystemLoad() SystemLoad {
	load := SystemLoad{}

	// CPU 使用率（简化：使用 load average）
	load.CPUPercent = getCPUUsage()

	// 内存使用率
	load.MemoryPercent = getMemoryUsage()

	return load
}

func getCPUUsage() float64 {
	// 读取 /proc/loadavg
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}

	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}

	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}

	// 转换为百分比（基于 CPU 核心数）
	numCPU := float64(runtime.NumCPU())
	percent := (load1 / numCPU) * 100
	if percent > 100 {
		percent = 100
	}

	return percent
}

func getMemoryUsage() float64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer file.Close()

	var memTotal, memAvailable int64

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}

		switch fields[0] {
		case "MemTotal:":
			memTotal = value
		case "MemAvailable:":
			memAvailable = value
		}
	}

	if memTotal == 0 {
		return 0
	}

	used := memTotal - memAvailable
	return float64(used) / float64(memTotal) * 100
}
