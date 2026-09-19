package vm_test

import (
	"testing"

	"k_cockpit/internal/vm"
)

// TestDiffIdentical 覆盖「没改」。
//
// 它不只是"没什么可看"——**一个没有变化的保存仍然会被下发**，而那会触发
// 节点侧一次完整的重新定义（几百毫秒的停机感知）。界面据此禁用保存按钮，
// 服务层也据此不下发。
func TestDiffIdentical(t *testing.T) {
	xml := "<domain>\n  <vcpu>2</vcpu>\n</domain>"
	d := vm.ExportDiffForTest(xml, xml)
	if !d.Identical {
		t.Errorf("相同的两份应当判为 identical，实际 +%d/-%d", d.Added, d.Removed)
	}
	if d.Added != 0 || d.Removed != 0 {
		t.Errorf("行数统计应为 0，实际 +%d/-%d", d.Added, d.Removed)
	}
}

// TestDiffTrailingNewlineIsNotAChange 覆盖**行尾换行不算改动**。
//
// `strings.Split("a\n", "\n")` 会得到 ["a", ""]——不去掉末尾空串的话，
// 「只是多了一个换行」会显示成"新增了一行"，而用户会去找那一行在哪。
func TestDiffTrailingNewlineIsNotAChange(t *testing.T) {
	d := vm.ExportDiffForTest("<domain/>", "<domain/>\n")
	if !d.Identical {
		t.Errorf("末尾换行不该算改动，实际 +%d/-%d", d.Added, d.Removed)
	}
}

// TestDiffCRLFIsNotAChange 覆盖 CRLF。
//
// 用户从 Windows 上复制过来的文本带 \r\n。不归一化的话，整份定义会显示成
// "全部行都变了"——而那个 diff 没有任何信息量，还会掩盖真正的改动。
func TestDiffCRLFIsNotAChange(t *testing.T) {
	d := vm.ExportDiffForTest("<domain>\n  <vcpu>2</vcpu>\n</domain>",
		"<domain>\r\n  <vcpu>2</vcpu>\r\n</domain>")
	if !d.Identical {
		t.Errorf("只有换行风格不同时不该算改动，实际 +%d/-%d", d.Added, d.Removed)
	}
}

// TestDiffAddAndRemove 覆盖常见的增删。
func TestDiffAddAndRemove(t *testing.T) {
	oldXML := "<domain>\n  <vcpu>2</vcpu>\n</domain>"
	newXML := "<domain>\n  <vcpu>4</vcpu>\n  <memory>4096</memory>\n</domain>"

	d := vm.ExportDiffForTest(oldXML, newXML)
	if d.Identical {
		t.Fatal("确有改动")
	}
	if d.Added != 2 || d.Removed != 1 {
		t.Errorf("应为 +2/-1，实际 +%d/-%d", d.Added, d.Removed)
	}

	// 逐行种类要对上：这是用户实际看的东西。
	kinds := map[string]int{}
	for _, l := range d.Lines {
		kinds[l.Kind]++
	}
	if kinds["del"] != 1 || kinds["add"] != 2 {
		t.Errorf("行种类不对: %+v", kinds)
	}
	// 未变的行要出现在结果里——**上下文是判断"改在哪"的关键**，
	// 只给出变化行会让用户看不出它在文件的哪个位置。
	if kinds["same"] != 2 {
		t.Errorf("应保留未变行作为上下文，实际 %d 行", kinds["same"])
	}
}

// TestDiffLineNumbers 覆盖行号。
//
// 用户要拿着行号回到编辑器里去找那一处，因此两侧的行号都要给——只给一侧
// 会让他按新行号去旧文件里找，而那是错的。
func TestDiffLineNumbers(t *testing.T) {
	oldXML := "a\nb\nc"
	newXML := "a\nX\nc"
	d := vm.ExportDiffForTest(oldXML, newXML)

	var del, add *int
	for i := range d.Lines {
		switch d.Lines[i].Kind {
		case "del":
			del = &d.Lines[i].OldNo
		case "add":
			add = &d.Lines[i].NewNo
		}
	}
	if del == nil || *del != 2 {
		t.Errorf("删除行应带旧侧行号 2，实际 %v", del)
	}
	if add == nil || *add != 2 {
		t.Errorf("新增行应带新侧行号 2，实际 %v", add)
	}
}

// TestDiffEmptyOld 覆盖从空到有（以及反向）。
func TestDiffEmptyOld(t *testing.T) {
	d := vm.ExportDiffForTest("", "a\nb")
	if d.Added != 2 || d.Removed != 0 {
		t.Errorf("从空到两行应为 +2/-0，实际 +%d/-%d", d.Added, d.Removed)
	}
	if d.Identical {
		t.Error("不该判为相同")
	}

	back := vm.ExportDiffForTest("a\nb", "")
	if back.Removed != 2 || back.Added != 0 {
		t.Errorf("清空应为 +0/-2，实际 +%d/-%d", back.Added, back.Removed)
	}

	both := vm.ExportDiffForTest("", "")
	if !both.Identical {
		t.Error("两份都空应判为相同")
	}
}

// TestDiffEveryLineAccountedFor 覆盖**不丢行**。
//
// diff 最容易出的错是"少报了一行"——而它的表现与"那行本来就没变"一模一样，
// 用户不会发现。因此这里做一个穷尽检查：结果里的每一行都必须能在某一侧
// 找到出处。
func TestDiffEveryLineAccountedFor(t *testing.T) {
	oldXML := "a\nb\nc\nd\ne"
	newXML := "a\nc\nX\ne\nf"

	d := vm.ExportDiffForTest(oldXML, newXML)

	oldSeen := map[string]int{}
	newSeen := map[string]int{}
	for _, l := range d.Lines {
		switch l.Kind {
		case "same":
			oldSeen[l.Text]++
			newSeen[l.Text]++
		case "del":
			oldSeen[l.Text]++
		case "add":
			newSeen[l.Text]++
		}
	}
	for _, want := range []string{"a", "b", "c", "d", "e"} {
		if oldSeen[want] != 1 {
			t.Errorf("旧侧的行 %q 出现 %d 次，期望 1 次——有行被漏掉了", want, oldSeen[want])
		}
	}
	for _, want := range []string{"a", "c", "X", "e", "f"} {
		if newSeen[want] != 1 {
			t.Errorf("新侧的行 %q 出现 %d 次，期望 1 次", want, newSeen[want])
		}
	}
}
