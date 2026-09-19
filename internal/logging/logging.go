// Package logging 提供服务端日志的级别、落盘、轮转与在线查看（F-9-02）。
//
// 本包建立在一处现实之上：**项目里已经有一百多处 `log.Printf`**，而它们
// 没有级别。把它们全部改写成带级别的调用是一次大范围且容易出错的改动，
// 而收益只是"能按级别过滤"。
//
// 因此采取**渐进**的做法：
//
//   - 接管 stdlib `log` 的输出（`log.SetOutput`），把那些行统一归为 INFO；
//   - 新代码使用本包的 `Debugf` / `Infof` / `Warnf` / `Errorf`；
//   - 级别过滤对带级别的行生效，INFO 是最低可见级别。
//
// 这样"按级别过滤"从第一天就可用于新代码，而不必等全量改造完成。
package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level 是日志级别。
type Level int

// 级别由低到高。
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// 级别的字符串表示。**定长 5 字符**，让日志正文在纵向上对齐——一列参差
// 的正文很难扫读，而扫读正是查日志时最主要的动作。
var levelName = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO ",
	LevelWarn:  "WARN ",
	LevelError: "ERROR",
}

// ParseLevel 解析级别名。
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, true
	case "info", "":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarn, true
	case "error":
		return LevelError, true
	}
	return LevelInfo, false
}

// String 返回级别名。
func (l Level) String() string {
	if n, ok := levelName[l]; ok {
		return strings.TrimSpace(n)
	}
	return "INFO"
}

// Options 是日志器的参数。
type Options struct {
	// Dir 是日志目录。为空则**只写内存与 stderr，不落盘**。
	//
	// 允许为空是刻意的：开发与测试环境不需要文件，而强制要求一个目录
	// 会让那些场景要么写进临时目录、要么失败。
	Dir string
	// FileName 是当前日志文件名。
	FileName string
	// MaxSizeBytes 是单文件上限，超过则轮转。
	MaxSizeBytes int64
	// KeepFiles 是保留的轮转文件数（不含当前文件）。
	KeepFiles int
	// RingSize 是内存中保留的最近行数，供在线查看。
	//
	// 有它才能让"看最近日志"这件事**不依赖磁盘**：用户点开日志时最常见
	// 的需求是"刚才那条错误是什么"，而那一条几乎总在最近的几百行里。
	RingSize int
	// Level 是当前级别。
	Level Level
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		FileName:     "kc.log",
		MaxSizeBytes: 32 << 20, // 32 MiB
		// 保留 5 个轮转文件：够覆盖"昨天到今天"，又不会让日志目录无限增长。
		KeepFiles: 5,
		// 2000 行大约覆盖一台忙碌机器几分钟的输出，也足够回答"刚才那条
		// 错误是什么"。再大就只是在内存里堆一份几乎不会被完整读到的副本。
		RingSize: 2000,
		Level:    LevelInfo,
	}
}

// Logger 是日志器。
type Logger struct {
	mu   sync.Mutex
	opts Options

	file     *os.File
	fileSize int64
	// ring 是最近的日志行（环形缓冲）。
	ring  []Entry
	start int
	count int

	// isSet 记录是否已经接管过 stdlib log，避免重复 SetOutput。
	// 重复接管会让输出写入死循环（本包写 stderr 时又触发 stdlib log），
	// 因此必须防止。
	tookOver bool

	// subs 是实时查看的订阅（见 subscribe.go）。为空表示没有人在看。
	subs map[*Subscription]struct{}
}

// Entry 是一条日志。
type Entry struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Line  string    `json:"line"`
}

// New 构造日志器并打开文件（若配了目录）。
func New(opts Options) (*Logger, error) {
	def := DefaultOptions()
	if opts.FileName == "" {
		opts.FileName = def.FileName
	}
	if opts.MaxSizeBytes <= 0 {
		opts.MaxSizeBytes = def.MaxSizeBytes
	}
	if opts.KeepFiles <= 0 {
		opts.KeepFiles = def.KeepFiles
	}
	if opts.RingSize <= 0 {
		opts.RingSize = def.RingSize
	}

	l := &Logger{opts: opts, ring: make([]Entry, opts.RingSize)}
	if opts.Dir != "" {
		if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
			return nil, fmt.Errorf("创建日志目录失败: %w", err)
		}
		if err := l.openFile(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// Writer 返回一个 io.Writer，供 stdlib log 接管使用。
//
// 由它进来的行**一律视为 INFO**：那些 `log.Printf` 调用点没有级别信息，
// 而猜一个级别（例如按关键字判断"失败"→ERROR）会产生大量误判——一条
// 写着"用户登录失败"的正常业务日志会被当成服务端错误，进而淹没真正的错误。
func (l *Logger) Writer() *Writer { return &Writer{l: l} }

// SetLevel 调整级别。
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.opts.Level = level
}

// Level 返回当前级别。
func (l *Logger) Level() Level {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.opts.Level
}

// Debugf / Infof / Warnf / Errorf 是带级别的写入。
func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

func (l *Logger) logf(level Level, format string, args ...any) {
	line := format
	if len(args) > 0 {
		line = fmt.Sprintf(format, args...)
	}
	l.write(level, line)
}

// write 写入一行：脱敏 → 内存 → 文件 → stderr。
func (l *Logger) write(level Level, line string) {
	// **脱敏在最前面**，先于任何落点。见 redact.go 的说明：写入时脱敏
	// 只留一个风险面，而读取时脱敏意味着文件里已经是明文。
	line = Redact(strings.TrimRight(line, "\n"))

	l.mu.Lock()
	defer l.mu.Unlock()

	if level < l.opts.Level {
		return
	}

	now := time.Now()
	text := now.Format("2006-01-02 15:04:05.000") + " " + levelName[level] + " " + line + "\n"

	// 内存环形缓冲：在线查看走它，不读磁盘。
	//
	// 写入位置分两段：未满时写在 count 处；满了之后写在 start 处（那里
	// 是最旧的一条）并把 start 前移。
	//
	// **不能用 `count % size` 当写入位置**：count 在写满之后就停在 size 上
	// 不再增长，于是那个取模结果恒为 0——所有新行都覆写第 0 个位置，而
	// 读取时看到的是一堆互不相关的旧行。这个错误在测试里表现为顺序错乱
	// 而不是"少了数据"，很容易被当成排序问题放过去。
	entry := Entry{At: now, Level: level.String(), Line: line}
	l.ring[l.writeAtLocked()] = entry

	// 实时查看的订阅。**非阻塞**——见 publishLocked 的说明：一个卡住的
	// 读者不能把日志写入（因而把整个服务）拖停。
	l.publishLocked(entry)

	if l.file != nil {
		l.writeFile(text)
	}
	// 始终同时写 stderr：服务以 systemd 之类的方式运行时，那是唯一能在
	// `journalctl` 里看到的地方，而排障时人最先去的就是那里。
	_, _ = os.Stderr.WriteString(text)
}

// writeAtLocked 返回下一个写入位置。调用方须持有锁。
func (l *Logger) writeAtLocked() int {
	size := len(l.ring)
	if l.count < size {
		pos := l.count
		l.count++
		return pos
	}
	pos := l.start
	l.start = (l.start + 1) % size
	return pos
}

func (l *Logger) writeFile(text string) {
	n, err := l.file.WriteString(text)
	if err != nil {
		// 写日志失败**不能递归记日志**——那会在磁盘满的时候变成死循环。
		_, _ = os.Stderr.WriteString("日志写入失败: " + err.Error() + "\n")
		return
	}
	l.fileSize += int64(n)
	if l.fileSize >= l.opts.MaxSizeBytes {
		l.rotate()
	}
}

// rotate 轮转当前文件。
//
// 命名用序号后缀 .1 .2 ...，而**不是**时间戳：序号让"哪个更旧"一眼可见，
// 而时间戳命名在按名字排序时会得到正确的顺序、却在人肉翻看时多一道换算。
func (l *Logger) rotate() {
	if l.file == nil {
		return
	}
	_ = l.file.Close()

	base := filepath.Join(l.opts.Dir, l.opts.FileName)
	// 丢掉最旧的那个。
	oldest := fmt.Sprintf("%s.%d", base, l.opts.KeepFiles)
	_ = os.Remove(oldest)
	// 依次后移。
	for i := l.opts.KeepFiles - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", base, i)
		dst := fmt.Sprintf("%s.%d", base, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Rename(src, dst)
		}
	}
	_ = os.Rename(base, base+".1")

	if err := l.openFile(); err != nil {
		_, _ = os.Stderr.WriteString("日志轮转后重开失败: " + err.Error() + "\n")
	}
}

func (l *Logger) openFile() error {
	path := filepath.Join(l.opts.Dir, l.opts.FileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("读取日志文件状态失败: %w", err)
	}
	l.file = f
	l.fileSize = info.Size()
	return nil
}

// Close 关闭文件。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Status 返回日志的运行状态。
type Status struct {
	Level        string `json:"level"`
	Dir          string `json:"dir,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	FileSize     int64  `json:"file_size"`
	MaxSizeBytes int64  `json:"max_size_bytes"`
	KeepFiles    int    `json:"keep_files"`
	// Rotated 是现有的轮转文件及其大小。
	Rotated []RotatedFile `json:"rotated"`
	// RingLines 是内存里可用于在线查看的行数。
	RingLines int `json:"ring_lines"`
	// Lines 是各级别的条数统计（仅内存范围）。
	//
	// 它回答的是"最近有没有错误"，而那是打开日志页时最想问的第一个问题。
	Lines map[string]int `json:"lines"`
}

// RotatedFile 是一个轮转文件。
type RotatedFile struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	ModTime   string `json:"mod_time"`
}

// Status 汇总当前状态。
func (l *Logger) Status() Status {
	l.mu.Lock()
	defer l.mu.Unlock()

	st := Status{
		Level:        l.opts.Level.String(),
		Dir:          l.opts.Dir,
		MaxSizeBytes: l.opts.MaxSizeBytes,
		KeepFiles:    l.opts.KeepFiles,
		RingLines:    l.count,
		Lines:        l.levelCountsLocked(),
	}
	if l.opts.Dir != "" {
		st.FilePath = filepath.Join(l.opts.Dir, l.opts.FileName)
		st.FileSize = l.fileSize
		st.Rotated = l.rotatedFilesLocked()
	}
	return st
}

func (l *Logger) levelCountsLocked() map[string]int {
	out := map[string]int{}
	for i := 0; i < l.count; i++ {
		out[l.ring[(l.start+i)%len(l.ring)].Level]++
	}
	return out
}

func (l *Logger) rotatedFilesLocked() []RotatedFile {
	if l.opts.Dir == "" {
		return nil
	}
	entries, err := os.ReadDir(l.opts.Dir)
	if err != nil {
		return nil
	}
	out := []RotatedFile{}
	prefix := l.opts.FileName + "."
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, RotatedFile{
			Name:      e.Name(),
			SizeBytes: info.Size(),
			ModTime:   info.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TailQuery 是在线查看的筛选条件。
type TailQuery struct {
	// Limit 是返回的最大行数（尾部）。
	Limit int
	// Level 为空表示不过滤。
	Level string
	// Keyword 是**不区分大小写**的子串匹配。
	Keyword string
}

// Tail 返回内存中最近的行（可按级别与关键字过滤）。
//
// **只读内存，不读磁盘**：在线查看要即时响应，而一个几百 MB 的日志文件
// 扫一遍会让页面卡上几秒。需要更早的内容时用 Export 下载。
func (l *Logger) Tail(q TailQuery) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	limit := q.Limit
	if limit <= 0 || limit > l.opts.RingSize {
		limit = 200
	}
	kw := strings.ToLower(strings.TrimSpace(q.Keyword))
	lvl := strings.TrimSpace(q.Level)

	// 从最新的往旧的扫，凑够 limit 就停——这样即使缓冲区很大也只扫
	// 它需要的那部分。
	out := make([]Entry, 0, limit)
	for i := l.count - 1; i >= 0 && len(out) < limit; i-- {
		e := l.ring[(l.start+i)%len(l.ring)]
		if lvl != "" && !strings.EqualFold(e.Level, lvl) {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(e.Line), kw) {
			continue
		}
		out = append(out, e)
	}
	// 反转为时间升序（最新的在最后），与人的阅读顺序一致。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ExportText 把磁盘上的日志（含轮转文件）拼成一段文本，供导出。
//
// 按**从旧到新**的顺序拼接：轮转文件里的是更早的内容。顺序反了的话，
// 读的人会以为错误发生在它之前的事情之后。
func (l *Logger) ExportText() (string, error) {
	l.mu.Lock()
	dir, name := l.opts.Dir, l.opts.FileName
	rotated := l.rotatedFilesLocked()
	l.mu.Unlock()

	if dir == "" {
		return "", fmt.Errorf("当前未启用日志落盘")
	}

	var b strings.Builder
	// 轮转文件：序号大的更旧。
	for i := len(rotated) - 1; i >= 0; i-- {
		data, err := os.ReadFile(filepath.Join(dir, rotated[i].Name))
		if err != nil {
			continue
		}
		b.Write(data)
	}
	if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
		b.Write(data)
	}
	return b.String(), nil
}

// Purge 删除全部轮转文件并清空当前文件，返回释放的字节数。
//
// **不删除当前文件本身**，只是截断它：删掉之后文件句柄会失效，而正在运行
// 的服务不会自动重建——下一次写日志就落到一个已经不存在的位置上，表现为
// "清理之后日志再也不出现了"。
func (l *Logger) Purge() (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.opts.Dir == "" {
		return 0, fmt.Errorf("当前未启用日志落盘")
	}

	var freed int64
	for _, f := range l.rotatedFilesLocked() {
		path := filepath.Join(l.opts.Dir, f.Name)
		if err := os.Remove(path); err == nil {
			freed += f.SizeBytes
		}
	}

	freed += l.fileSize
	if l.file != nil {
		if err := l.file.Truncate(0); err != nil {
			return freed, fmt.Errorf("清空日志文件失败: %w", err)
		}
		if _, err := l.file.Seek(0, 0); err != nil {
			return freed, fmt.Errorf("重置写入位置失败: %w", err)
		}
		l.fileSize = 0
	}
	return freed, nil
}
