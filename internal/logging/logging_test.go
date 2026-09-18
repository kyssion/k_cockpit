package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLogger(t *testing.T, opts Options) *Logger {
	t.Helper()
	opts.Dir = t.TempDir()
	l, err := New(opts)
	if err != nil {
		t.Fatalf("构造日志器失败: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// TestLevelFilters 覆盖级别过滤。
func TestLevelFilters(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelWarn, RingSize: 100})

	l.Debugf("调试信息")
	l.Infof("普通信息")
	l.Warnf("警告")
	l.Errorf("错误")

	got := l.Tail(TailQuery{})
	if len(got) != 2 {
		t.Fatalf("WARN 级别下应只剩 2 条，实际 %d 条: %+v", len(got), got)
	}
	if got[0].Level != "WARN" || got[1].Level != "ERROR" {
		t.Errorf("保留的应是 WARN 与 ERROR，实际 %s / %s", got[0].Level, got[1].Level)
	}
}

// TestStdlibWriterIsInfo 覆盖 stdlib log 的接管。
//
// 项目里已有一百多处 log.Printf，而它们没有级别信息。接管之后一律归为
// INFO——不按关键字猜级别，因为一条写着「用户登录失败」的正常业务日志
// 会被猜成 ERROR，进而淹没真正的错误。
func TestStdlibWriterIsInfo(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelInfo, RingSize: 100})

	w := l.Writer()
	if _, err := w.Write([]byte("来自 stdlib log 的一行\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got := l.Tail(TailQuery{})
	if len(got) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(got))
	}
	if got[0].Level != "INFO" {
		t.Errorf("级别应为 INFO，实际 %s", got[0].Level)
	}
	if got[0].Line != "来自 stdlib log 的一行" {
		t.Errorf("内容不对: %q", got[0].Line)
	}
}

// TestWriterBuffersPartialLines 覆盖跨多次 Write 的一行。
//
// io.Writer 的契约不保证一次调用就是一行，而 stdlib log 的实现细节会变。
// 不缓冲的话，一条被拆成两次 Write 的日志会变成两条不完整的记录。
func TestWriterBuffersPartialLines(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelInfo, RingSize: 100})
	w := l.Writer()

	_, _ = w.Write([]byte("前半"))
	_, _ = w.Write([]byte("后半\n"))

	got := l.Tail(TailQuery{})
	if len(got) != 1 || got[0].Line != "前半后半" {
		t.Errorf("跨 Write 的一行应被拼成一条，实际 %+v", got)
	}
}

// TestRedactionAppliesOnWrite 覆盖**写入时脱敏**这条核心决定。
//
// 断言的是磁盘上的内容，而不只是内存里的——如果有任何一条路径能把明文
// 写进文件，这个功能就等于没做。
func TestRedactionAppliesOnWrite(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelInfo, RingSize: 100})
	l.Infof("登录请求 password=hunter2 token=abc123")

	// 内存
	got := l.Tail(TailQuery{})
	if strings.Contains(got[0].Line, "hunter2") || strings.Contains(got[0].Line, "abc123") {
		t.Errorf("内存中残留明文: %q", got[0].Line)
	}

	// 磁盘
	data, err := os.ReadFile(filepath.Join(l.opts.Dir, l.opts.FileName))
	if err != nil {
		t.Fatalf("读取日志文件失败: %v", err)
	}
	if strings.Contains(string(data), "hunter2") || strings.Contains(string(data), "abc123") {
		t.Errorf("磁盘上残留明文——文件会被运维、备份、采集代理看到:\n%s", data)
	}
}

// TestTailFilterAndOrder 覆盖在线查看的筛选与顺序。
func TestTailFilterAndOrder(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelDebug, RingSize: 100})

	l.Infof("普通 1")
	l.Errorf("错误 A")
	l.Infof("普通 2")
	l.Errorf("错误 B")

	only := l.Tail(TailQuery{Level: "ERROR"})
	if len(only) != 2 {
		t.Fatalf("按 ERROR 过滤应得 2 条，实际 %d", len(only))
	}
	// 时间升序：最新的在最后，与阅读顺序一致。
	if !strings.Contains(only[0].Line, "错误 A") || !strings.Contains(only[1].Line, "错误 B") {
		t.Errorf("顺序应为时间升序，实际 %q / %q", only[0].Line, only[1].Line)
	}

	kw := l.Tail(TailQuery{Keyword: "普通"})
	if len(kw) != 2 {
		t.Errorf("按关键字过滤应得 2 条，实际 %d", len(kw))
	}
}

// TestRingKeepsOnlyRecent 覆盖环形缓冲的上限。
func TestRingKeepsOnlyRecent(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelInfo, RingSize: 5})

	for i := 0; i < 20; i++ {
		l.Infof("第 %d 条", i)
	}

	got := l.Tail(TailQuery{Limit: 100})
	if len(got) != 5 {
		t.Fatalf("应只保留 5 条，实际 %d", len(got))
	}
	if got[0].Line != "第 15 条" || got[4].Line != "第 19 条" {
		t.Errorf("应保留最近 5 条，实际 %q ~ %q", got[0].Line, got[4].Line)
	}
}

// TestPurgeTruncatesInsteadOfRemoving 覆盖清理的语义。
//
// **不能删除当前文件本身**：删掉之后已打开的句柄指向一个不存在的 inode，
// 而正在运行的服务不会自动重建——下一次写日志就落到虚空里，表现为
// "清理之后日志再也不出现了"，而那时恰恰最需要日志。
func TestPurgeTruncatesInsteadOfRemoving(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelInfo, RingSize: 100})
	l.Infof("清理前的一条")

	freed, err := l.Purge()
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if freed <= 0 {
		t.Errorf("应报告释放的字节数，实际 %d", freed)
	}

	// 文件必须还在。
	path := filepath.Join(l.opts.Dir, l.opts.FileName)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("清理后日志文件不该被删除（句柄会失效）: %v", err)
	}

	// 而且还能继续写——这是最要紧的一条。
	l.Infof("清理后的一条")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(string(data), "清理后的一条") {
		t.Errorf("清理之后日志写不进去了:\n%s", data)
	}
	if strings.Contains(string(data), "清理前的一条") {
		t.Errorf("清理前的旧内容仍在:\n%s", data)
	}
}

// TestStatusCountsLevels 覆盖状态里的级别统计。
//
// 它回答的是打开日志页时最想问的第一个问题：「最近有没有错误」。
func TestStatusCountsLevels(t *testing.T) {
	l := newTestLogger(t, Options{Level: LevelDebug, RingSize: 100})
	l.Infof("a")
	l.Infof("b")
	l.Errorf("c")

	st := l.Status()
	if st.Lines["INFO"] != 2 || st.Lines["ERROR"] != 1 {
		t.Errorf("级别统计不对: %+v", st.Lines)
	}
	if st.RingLines != 3 {
		t.Errorf("内存行数 = %d, 期望 3", st.RingLines)
	}
	if st.Level != "DEBUG" {
		t.Errorf("级别回显 = %q", st.Level)
	}
}

// TestNoDirMeansNoFile 覆盖"不落盘"这个合法配置。
//
// 开发与测试环境不需要文件，而强制要求一个目录会让那些场景要么写进临时
// 目录、要么直接失败。
func TestNoDirMeansNoFile(t *testing.T) {
	l, err := New(Options{Level: LevelInfo, RingSize: 10})
	if err != nil {
		t.Fatalf("不配目录时应能构造: %v", err)
	}
	defer func() { _ = l.Close() }()

	l.Infof("只进内存")
	if len(l.Tail(TailQuery{})) != 1 {
		t.Error("不落盘时仍应可用（内存查看）")
	}
	if _, err := l.ExportText(); err == nil {
		t.Error("未落盘时导出应给出明确错误，而不是返回空内容")
	}
}
