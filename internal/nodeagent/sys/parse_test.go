package sys

import (
	"os"
	"testing"
)

// fixture 读 testdata 下的真机脱敏基料。
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读基料 %s: %v", name, err)
	}
	return b
}

// TestParseVirshList 真机输出：两台关机域。名称含数字后缀、Id 为 "-"。
func TestParseVirshList(t *testing.T) {
	entries := parseVirshList(string(fixture(t, "virsh_list.txt")))
	if len(entries) != 2 {
		t.Fatalf("应解析出 2 台域，实际 %d：%+v", len(entries), entries)
	}
	if entries[0].Name != "vm3ohvs29a" || entries[0].ID != -1 {
		t.Errorf("第一台应为关机的 vm3ohvs29a，实际 %+v", entries[0])
	}
	if DomainState(entries[0].State) != "stopped" {
		t.Errorf("shut off 应映射为 stopped，实际 %s", DomainState(entries[0].State))
	}
}

// TestParseLsblk 真机输出：vda 是系统盘（根挂载在分区上）、loop0 挂着
// 用户存储、sda 是 iso9660 安装盘。
func TestParseLsblk(t *testing.T) {
	var out struct {
		BlockDevices []BlockDevice `json:"blockdevices"`
	}
	if err := jsonUnmarshal(fixture(t, "lsblk.json"), &out); err != nil {
		t.Fatalf("lsblk 解析: %v", err)
	}
	var vda, loop0, sda *BlockDevice
	for i := range out.BlockDevices {
		switch out.BlockDevices[i].Name {
		case "vda":
			vda = &out.BlockDevices[i]
		case "loop0":
			loop0 = &out.BlockDevices[i]
		case "sda":
			sda = &out.BlockDevices[i]
		}
	}
	if vda == nil || len(vda.Children) == 0 {
		t.Fatalf("vda 应存在且带分区")
	}
	if loop0 == nil || loop0.MountPoint == "" {
		t.Errorf("loop0 应处于挂载状态")
	}
	if !HasPartitionsOrFS(*vda) {
		t.Errorf("vda 有分区，HasData 口径应为 true")
	}
	if !SubtreeMounted(*vda) {
		t.Errorf("vda 的分区挂着根，应为 mounted")
	}
	if sda != nil && sda.FSType == "iso9660" && len(sda.Children) > 0 {
		if !HasPartitionsOrFS(*sda) {
			t.Errorf("sda 带分区，HasData 应为 true")
		}
	}
}

// TestCPUBetween 真机 /proc/stat 的两次读数之间应能算出占用率（0-100）。
func TestCPUBetween(t *testing.T) {
	a, err := parseProcStat(fixture(t, "proc_stat.txt"))
	if err != nil {
		t.Fatalf("解析 /proc/stat: %v", err)
	}
	if a.Total == 0 || len(a.PerCoreTotal) == 0 {
		t.Fatalf("采样为空: %+v", a)
	}
	// 构造一份"走了一些 tick"的后续采样验证公式方向：idle 增长慢于 total
	// 时占用率为正。
	b := &CPUSample{Total: a.Total + 1000, Idle: a.Idle + 100,
		PerCoreTotal: a.PerCoreTotal, PerCoreIdle: a.PerCoreIdle}
	if p := CPUPercentBetween(a, b); p < 80 || p > 95 {
		t.Errorf("构造样本的占用率应在 85%% 附近，实际 %.1f", p)
	}
}

// TestParseMemInfo 真机 meminfo：7.7G 机器的总量应在 7400~8100 MB。
func TestParseMemInfo(t *testing.T) {
	m, err := parseMemInfo(fixture(t, "meminfo.txt"))
	if err != nil {
		t.Fatalf("解析 meminfo: %v", err)
	}
	if m.TotalMB < 7400 || m.TotalMB > 8100 {
		t.Errorf("TotalMB=%d 超出合理范围", m.TotalMB)
	}
	if m.UsedMB <= 0 || m.UsedMB >= m.TotalMB {
		t.Errorf("UsedMB=%d 不合理", m.UsedMB)
	}
}

// TestParseNetDev 真机 /proc/net/dev：应包含 lo 与 enp0s1，且计数为累计值。
func TestParseNetDev(t *testing.T) {
	m, err := parseCounters(fixture(t, "net_dev.txt"), 1, 9)
	if err != nil {
		t.Fatalf("解析 net_dev: %v", err)
	}
	if _, ok := m["lo"]; !ok {
		t.Error("应包含 lo")
	}
	enp, ok := m["enp0s1"]
	if !ok {
		t.Fatal("应包含 enp0s1")
	}
	if enp.ReadBytes <= 0 {
		t.Errorf("累计收字节应 > 0，实际 %d", enp.ReadBytes)
	}
}

// TestParseNeighbors 真机 ip neighbor 输出应解析出网口与状态。
func TestParseNeighbors(t *testing.T) {
	out := "198.51.100.1 dev enp0s1 lladdr 52:54:00:11:22:33 REACHABLE\n" +
		"198.51.100.99 dev enp0s1  FAILED\n"
	entries, err := parseNeighbors(out)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(entries))
	}
	if entries[0].Interface != "enp0s1" || entries[0].MAC == "" || entries[0].State != "reachable" {
		t.Errorf("第一条解析不符: %+v", entries[0])
	}
	if entries[1].State != "failed" {
		t.Errorf("FAILED 应保留（它意味着二层不通），实际 %+v", entries[1])
	}
}

// TestParseSS 建立 中的连接应解析出本地端口与进程名。
func TestParseSS(t *testing.T) {
	out := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" +
		"ESTAB 0 0 198.51.100.10:22 198.51.100.1:52344 users:((\"sshd\",pid=800,fd=4))\n"
	conns, err := parseEstablished(out)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(conns) != 1 || conns[0].LocalPort != 22 {
		t.Fatalf("解析结果不符: %+v", conns)
	}
	if conns[0].Process != "sshd" {
		t.Errorf("进程名应为 sshd，实际 %q", conns[0].Process)
	}
	if got := remoteOf(conns[0]); got != "198.51.100.1:52344" {
		t.Errorf("远端地址不符: %s", got)
	}
}

// TestParseLeases dnsmasq 租约行解析。
func TestParseLeases(t *testing.T) {
	entries := parseLeases([]string{
		"1759500000 52:54:00:aa:bb:cc 198.51.100.123 vm3ohvs29a 01:52:54:00:aa:bb:cc",
		"* 52:54:00:dd:ee:ff 198.51.100.124 * *",
	})
	if len(entries) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(entries))
	}
	if entries[0].Hostname != "vm3ohvs29a" || entries[0].ExpiresUnix != 1759500000 {
		t.Errorf("第一条解析不符: %+v", entries[0])
	}
	if entries[1].Hostname != "" || entries[1].ClientID != "" {
		t.Errorf("星号应转为空串: %+v", entries[1])
	}
}

// TestParseDomStats domstats 行格式的累计计数合并。
func TestParseDomStats(t *testing.T) {
	s := parseDomStats(`vm3ohvs29a: cpu.time=123000000000
vm3ohvs29a: vcpu.current=2
vm3ohvs29a: balloon.current=2097152
vm3ohvs29a: net.0.rx.bytes=1000
vm3ohvs29a: net.0.tx.bytes=2000
vm3ohvs29a: net.1.rx.bytes=3000
vm3ohvs29a: block.0.rd.bytes=500
vm3ohvs29a: block.0.wr.bytes=700
`)
	if s.CPUTimeNS != 123000000000 || s.VCPUs != 2 {
		t.Errorf("CPU 字段不符: %+v", s)
	}
	if s.NetRxBytes != 4000 || s.NetTxBytes != 2000 {
		t.Errorf("网口计数应合并: %+v", s)
	}
	if s.DiskReadBytes != 500 || s.DiskWriteBytes != 700 {
		t.Errorf("磁盘计数不符: %+v", s)
	}
	if !s.Running {
		t.Error("有 vcpu 记录应判为 running")
	}
}

// TestParseDomainDisks dumpxml 的磁盘提取：dev/bus/source/设备类别。
func TestParseDomainDisks(t *testing.T) {
	xml := `<domain type='kvm'>
  <os><type arch='aarch64' machine='virt'>hvm</type></os>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='/var/lib/k_cockpit/images/vm1.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/k_cockpit/ISO/ubuntu.iso'/>
      <target dev='sda' bus='scsi'/>
    </disk>
  </devices>
</domain>`
	disks := ParseDomainDisks(xml)
	if len(disks) != 2 {
		t.Fatalf("应有 2 块，实际 %d", len(disks))
	}
	if disks[0].Dev != "vda" || disks[0].Bus != "virtio" || disks[0].Format != "qcow2" || disks[0].IsCDROM {
		t.Errorf("第一块解析不符: %+v", disks[0])
	}
	if !disks[1].IsCDROM || disks[1].Dev != "sda" {
		t.Errorf("第二块应为 cdrom: %+v", disks[1])
	}
	if MachineTypeOf(xml) != "virt" {
		t.Errorf("机型应为 virt，实际 %s", MachineTypeOf(xml))
	}
}
