package vm

import "strings"

// XMLDiff 是一次编辑的影响范围。
//
// **必须在用户保存之前给出**。XML 编辑与项目里其它改动有一个根本不同：
// 别处用户改的是「字段」，而这里他改的是**整份定义**——他能动的范围没有
// 边界。不给 diff 的话，他看到的是一大段 XML，而要判断的是"我这一改会动到
// 什么"，那两件事对不上。
type XMLDiff struct {
	// Lines 是逐行结果：' ' 未变、'-' 删除、'+' 新增。
	//
	// 用统一 diff 的三态而不是两份完整文本并排：并排要求人自己去比对，
	// 而"哪些行变了"正是他要看的东西。
	Lines []DiffLine `json:"lines"`
	// Added / Removed 是行数统计。
	//
	// 它是给"只想扫一眼"的人的：一个 +0/-0 的改动不用细看，而 +120/-3
	// 的改动需要逐行过。没有这两个数字，用户只能靠翻完整个列表来判断。
	Added   int `json:"added"`
	Removed int `json:"removed"`
	// Identical 为 true 表示两份完全一样。
	//
	// 它不只是"没什么可看"——**一个没有变化的保存仍然会被下发**，而那会
	// 触发节点侧一次完整的重新定义。因此界面应当据此禁用保存按钮。
	Identical bool `json:"identical"`
}

// DiffLine 是 diff 的一行。
type DiffLine struct {
	// Kind 取 same / add / del。
	Kind string `json:"kind"`
	// OldNo / NewNo 是行号（0 表示该侧没有这一行）。
	OldNo int    `json:"old_no,omitempty"`
	NewNo int    `json:"new_no,omitempty"`
	Text  string `json:"text"`
}

// diffXML 逐行比较两份定义。
//
// **只做行级比较，不解析 XML 结构**。这是刻意的：解析成树再比较会得到
// "属性顺序变了"这类噪声（而在 XML 里属性顺序无意义），而用户真正要看的是
// "我改了哪几行"。行级比较与他眼睛看到的东西一一对应。
//
// 算法是 LCS 的动态规划版本。域定义通常几百行，O(n·m) 完全够用；而换成
// 更省内存的变体只会让这段代码更难读。
func diffXML(oldXML, newXML string) *XMLDiff {
	// **保留行尾的空行差异**：`strings.Split` 会把 "a\n" 分成 ["a", ""]，
	// 而把末尾空串去掉才能让"只是多了一个换行"不显示成一行新增。
	a := splitLines(oldXML)
	b := splitLines(newXML)

	n, m := len(a), len(b)
	// lcs[i][j] = a[i:] 与 b[j:] 的最长公共子序列长度。
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	out := &XMLDiff{Lines: []DiffLine{}}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out.Lines = append(out.Lines, DiffLine{
				Kind: "same", OldNo: i + 1, NewNo: j + 1, Text: a[i],
			})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out.Lines = append(out.Lines, DiffLine{Kind: "del", OldNo: i + 1, Text: a[i]})
			out.Removed++
			i++
		default:
			out.Lines = append(out.Lines, DiffLine{Kind: "add", NewNo: j + 1, Text: b[j]})
			out.Added++
			j++
		}
	}
	for ; i < n; i++ {
		out.Lines = append(out.Lines, DiffLine{Kind: "del", OldNo: i + 1, Text: a[i]})
		out.Removed++
	}
	for ; j < m; j++ {
		out.Lines = append(out.Lines, DiffLine{Kind: "add", NewNo: j + 1, Text: b[j]})
		out.Added++
	}
	out.Identical = out.Added == 0 && out.Removed == 0
	return out
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ExportDiffForTest 导出 diff 函数供测试使用。
//
// diff 的正确性很难从界面上验证（"少报了一行"看起来与"那行本来就没变"
// 一样），因此它必须能被单独测。
func ExportDiffForTest(oldXML, newXML string) *XMLDiff {
	return diffXML(oldXML, newXML)
}
