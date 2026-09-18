package logging

import "strings"

// Writer 把 stdlib `log` 的输出接进 Logger。
//
// 为什么需要它：项目里已有一百多处 `log.Printf`，而它们没有级别信息。
// 逐个改写是一次大范围且容易出错的改动，而收益只是"能按级别过滤"。
// 接管输出则让那些行**立刻**进入同一个落点（同一份文件、同一个环形缓冲、
// 同一套脱敏规则），而不必等改造完成。
//
// 由它进来的行一律视为 **INFO**。不按关键字猜级别（例如"失败"→ERROR），
// 因为那会产生大量误判：一条写着「用户登录失败」的正常业务日志会被当成
// 服务端错误，进而淹没真正的错误——而"从一堆假错误里找出真错误"正是
// 级别过滤要解决的问题。
type Writer struct {
	l *Logger
	// buf 累积一行直到换行符。
	//
	// stdlib log 每次输出是一整行，但 io.Writer 的契约不保证这一点——
	// 不缓冲的话，一条被拆成两次 Write 的日志会变成两条。
	buf strings.Builder
}

// Write 实现 io.Writer。
func (w *Writer) Write(p []byte) (int, error) {
	n := len(p)
	s := string(p)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			w.buf.WriteString(s)
			break
		}
		w.buf.WriteString(s[:i])
		// 交给 Logger 做脱敏与分发；级别按 INFO 处理（见类型注释）。
		w.l.logf(LevelInfo, "%s", w.buf.String())
		w.buf.Reset()
		s = s[i+1:]
	}
	// **返回原始长度**而不是"实际处理了多长"：stdlib log 据此判断是否
	// 写入出错，返回短写会让它打印"日志写入失败"，而那是一条假警报。
	return n, nil
}
