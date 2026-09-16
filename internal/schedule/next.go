package schedule

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/model"
)

// errUnknownAction 表示记录里的动作不在支持范围内。
var errUnknownAction = errors.New("不支持的定时任务动作")

// NextAfter 计算给定时刻之后的**下一次**执行时刻；不会再执行时返回 nil。
//
// 时间按服务器本地时区计算。这是一个明确的取舍：跨时区用户的「凌晨 3 点」
// 会有歧义，而引入时区配置会让部署多一个必填项。当前部署形态是单节点自用，
// 本地时区即用户的时区。
func NextAfter(sch *model.VMSchedule, from time.Time) *time.Time {
	if !sch.Enabled {
		return nil
	}
	hour, minute := parseRunAt(sch.RunAt)

	switch sch.ScheduleType {
	case model.ScheduleTypeOnce:
		// 一次性任务没有「下一次」。这里返回 nil 会把 next_run_at 置空，
		// 但记录**仍保持 enabled**——界面据 last_result 显示「已完成」。
		//
		// 不把它标记为停用：那会让一次性的定时任务在界面上看起来像被用户
		// 关掉了，而用户只是想看它有没有跑过。
		return nil

	case model.ScheduleTypeDaily:
		at := time.Date(from.Year(), from.Month(), from.Day(), hour, minute, 0, 0, from.Location())
		if !at.After(from) {
			at = at.AddDate(0, 0, 1)
		}
		return &at

	case model.ScheduleTypeWeekly:
		days := parseWeekdays(sch.Weekdays)
		if len(days) == 0 {
			// 每周模式却没勾任何一天：返回 nil 让它停下来。
			// 继续算下去只能编一个默认值，而「猜用户想要哪天」比不执行更危险。
			return nil
		}
		// 从今天起往后找最多 7 天（多查一天覆盖「今天已过点」的情况）。
		for i := 0; i <= 7; i++ {
			day := from.AddDate(0, 0, i)
			if !days[isoWeekday(day)] {
				continue
			}
			at := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())
			if at.After(from) {
				return &at
			}
		}
		return nil
	}

	return nil
}

// parseRunAt 解析 `HH:MM` 形式的执行时刻。
//
// 解析失败时回落到 0 点整而不是报错：一条存着非法时刻的记录不该让整个调度
// 循环停下来。0 点是一个明确的时刻，比「不执行」更容易被察觉。
func parseRunAt(raw *string) (hour, minute int) {
	if raw == nil {
		return 0, 0
	}
	parts := strings.SplitN(strings.TrimSpace(*raw), ":", 2)
	if len(parts) == 0 {
		return 0, 0
	}

	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return 0, 0
	}
	if len(parts) == 1 {
		return h, 0
	}
	m, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || m < 0 || m > 59 {
		return h, 0
	}
	return h, m
}

// parseWeekdays 解析 `1,3,5` 形式的星期集合。
//
// 取值 1=周一 … 7=周日（ISO 8601），与界面上的勾选顺序一致。用 ISO 而不是
// Go 的 0=周日：后者会让「周日」在界面上排在第一个，而用户按自然顺序读
// 「周一到周日」，两套编号混用是这类功能最常见的错位来源。
func parseWeekdays(raw *string) map[int]bool {
	out := map[int]bool{}
	if raw == nil {
		return out
	}
	for _, part := range strings.Split(*raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > 7 {
			continue
		}
		out[n] = true
	}
	return out
}

// isoWeekday 返回 ISO 8601 的星期编号：1=周一 … 7=周日。
func isoWeekday(t time.Time) int {
	wd := int(t.Weekday())
	if wd == 0 {
		return 7
	}
	return wd
}
