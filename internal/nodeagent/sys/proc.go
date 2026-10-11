package sys

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CPUSample 是 /proc/stat 的一次采样（自开机以来的累计 jiffies）。
type CPUSample struct {
	// Total 是全部列的合计（含 idle/iowait）。
	Total uint64
	// Idle 是 idle + iowait——「什么都没做」的时间。
	Idle uint64
	// PerCore 是每个逻辑核心的合计与空闲（与 Total 同口径，下标即核心号）。
	PerCoreTotal []uint64
	PerCoreIdle  []uint64
}

// ReadCPUSample 读取一次 /proc/stat。
func ReadCPUSample() (*CPUSample, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil, err
	}
	return parseProcStat(b)
}

// parseProcStat 解析 /proc/stat 内容。
func parseProcStat(b []byte) (*CPUSample, error) {
	s := &CPUSample{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu ") {
			total, idle, err := cpuLine(line)
			if err != nil {
				return nil, err
			}
			s.Total, s.Idle = total, idle
			continue
		}
		if strings.HasPrefix(line, "cpu") {
			t, idle, err := cpuLine(line)
			if err != nil {
				return nil, err
			}
			s.PerCoreTotal = append(s.PerCoreTotal, t)
			s.PerCoreIdle = append(s.PerCoreIdle, idle)
		}
	}
	return s, sc.Err()
}

// cpuLine 解析一行 "cpuN v1 v2 ..."：返回合计与 idle+iowait。
func cpuLine(line string) (total, idle uint64, err error) {
	fields := strings.Fields(line)
	for i, f := range fields[1:] {
		v, perr := strconv.ParseUint(f, 10, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("/proc/stat %s: %w", fields[0], perr)
		}
		total += v
		// 列 3 = idle、列 4 = iowait（0 起算的用户/系统/ nice 之后）。
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return total, idle, nil
}

// CPUPercentBetween 由两次采样计算占用率（0-100）。
//
// 两次采样间隔内没有走过任何 tick（极端空闲或时钟粒度）时返回 0 而不是
// 除零错误——0% 与「算不出来」在这里都呈现为空闲，误差可接受。
func CPUPercentBetween(a, b *CPUSample) float64 {
	dt := b.Total - a.Total
	if dt == 0 {
		return 0
	}
	di := b.Idle - a.Idle
	return float64(dt-di) / float64(dt) * 100
}

// MemInfo 是 /proc/meminfo 的关键字段（单位 MB）。
type MemInfo struct {
	TotalMB int
	// UsedMB = MemTotal - MemAvailable：available 才是「还能给应用多少」，
	// free 把 cache 都算成占用了。
	UsedMB int
}

// ReadMemInfo 读取 /proc/meminfo。
func ReadMemInfo() (*MemInfo, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return nil, err
	}
	return parseMemInfo(b)
}

// parseMemInfo 解析 /proc/meminfo 内容。
func parseMemInfo(b []byte) (*MemInfo, error) {
	total, avail := 0, 0
	haveTotal, haveAvail := false, false
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, perr := strconv.Atoi(fields[1])
		if perr != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = v, true
		case "MemAvailable:":
			avail, haveAvail = v, true
		}
	}
	if !haveTotal || !haveAvail {
		return nil, fmt.Errorf("/proc/meminfo 缺少 MemTotal/MemAvailable")
	}
	return &MemInfo{TotalMB: total / 1024, UsedMB: (total - avail) / 1024}, nil
}

// ReadLoadAvg 读取 1/5/15 分钟负载。
func ReadLoadAvg() (l1, l5, l15 float64, err error) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return 0, 0, 0, fmt.Errorf("/proc/loadavg 格式异常")
	}
	vals := make([]float64, 3)
	for i := 0; i < 3; i++ {
		if vals[i], err = strconv.ParseFloat(fields[i], 64); err != nil {
			return 0, 0, 0, err
		}
	}
	return vals[0], vals[1], vals[2], nil
}

// ReadUptimeSeconds 读取宿主机开机时长（秒）。
func ReadUptimeSeconds() (int64, error) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("/proc/uptime 为空")
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

// DevCounter 是一块网卡或磁盘的自开机累计字节数。
type DevCounter struct {
	Name       string
	ReadBytes  int64 // 网卡为入向，磁盘为读
	WriteBytes int64 // 网卡为出向，磁盘为写
}

// ReadNetDevCounters 读取 /proc/net/dev（累计字节，含 lo——调用方过滤）。
//
// 字段布局（空格切分后 0 起）：0=网卡名、1=收字节、9=发字节。
func ReadNetDevCounters() (map[string]DevCounter, error) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	return parseCounters(b, 1, 9)
}

// ReadDiskStatsCounters 读取 /proc/diskstats 的读/写扇区合计（换算成字节）。
//
// 取整盘（如 sda/vda），分区（sda1）一并计入但调用方按需挑选。
func ReadDiskStatsCounters(sectorSize int64) (map[string]DevCounter, error) {
	b, err := os.ReadFile("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	m, err := parseCounters(b, 5, 9)
	if err != nil {
		return nil, err
	}
	for name, c := range m {
		c.ReadBytes *= sectorSize
		c.WriteBytes *= sectorSize
		m[name] = c
	}
	return m, nil
}

// parseCounters 解析计数文件：rxIdx/txIdx 为空格切分后的字段下标。
func parseCounters(b []byte, rxIdx, txIdx int) (map[string]DevCounter, error) {
	out := map[string]DevCounter{}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) <= rxIdx || len(fields) <= txIdx {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		rx, e1 := strconv.ParseInt(fields[rxIdx], 10, 64)
		tx, e2 := strconv.ParseInt(fields[txIdx], 10, 64)
		if e1 != nil || e2 != nil {
			continue
		}
		out[name] = DevCounter{Name: name, ReadBytes: rx, WriteBytes: tx}
	}
	return out, sc.Err()
}

// jsonUnmarshal 是测试与解析共用的最小 JSON 入口（避免测试文件直接依赖
// encoding/json 的样板）。
func jsonUnmarshal(b []byte, v any) error { return jsonUnmarshalImpl(b, v) }
