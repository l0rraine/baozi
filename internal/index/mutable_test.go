package index

import (
	"path/filepath"
	"testing"
)

// 一个假的小卷：根 → 目录 → 文件，全部带 FRN。
// 下标手工算好，好让"服务端 append 的顺序"和"主进程重放的顺序"能对上。
func testTable(t *testing.T) (*Mutable, map[string]uint64) {
	t.Helper()
	tab := &Table{
		Names: make([]byte, 0, 4096),
		Py:    make(map[int32]uint64, 8),
	}
	m := NewMutable(tab, NewFRNMap(8))
	// 0 卷根，1 目录 docs，2 文件报告.txt
	rootID := m.Add(`C:\`, -1, KindDir, 0, 0, 5)
	docs := m.Add("docs", rootID, KindDir, 0, 0, 100)
	report := m.Add("报告.txt", docs, KindFile, 0, 0, 101)
	if rootID != 0 || docs != 1 || report != 2 {
		t.Fatalf("下标不对: %d %d %d", rootID, docs, report)
	}
	return m, map[string]uint64{"root": 5, "docs": 100, "report": 101}
}

func TestMutableAddAndPath(t *testing.T) {
	m, frn := testTable(t)
	tab := m.Table()

	id := m.Add("新文件.log", 1, KindFile, 0, 0, 200)
	if id != 3 {
		t.Fatalf("追加下标应为 3，实际 %d", id)
	}
	if got := tab.PathOf(id); got != `C:\docs\新文件.log` {
		t.Fatalf("新条目路径错误: %s", got)
	}
	// 追加不能动到已有下标
	if tab.Name(2) != "报告.txt" || tab.PathOf(2) != `C:\docs\报告.txt` {
		t.Fatal("追加破坏了已有条目")
	}
	if _, ok := m.frn.Get(frn["docs"]); !ok {
		t.Fatal("FRN 映射丢了 docs")
	}
}

func TestMutableKillAndDead(t *testing.T) {
	m, frn := testTable(t)
	tab := m.Table()

	// 删掉 docs 目录：它下面没有 USN 记录，子条目必须靠上溯失效
	op, ok := m.Apply(Change{FRN: frn["docs"], Delete: true}, Rule{})
	if !ok || op.Op != OpDelete || op.Idx != 1 {
		t.Fatalf("删除没产生正确的指令: %+v ok=%v", op, ok)
	}
	if got := tab.PathOf(2); got != "" {
		t.Fatalf("父被删后子条目还拼得出路径: %s", got)
	}
	if !tab.Dead(2) {
		t.Fatal("父被删后子条目应该判定失效")
	}
	if tab.Dead(0) {
		t.Fatal("卷根本身不该失效")
	}
	if m.Dead() != 1 {
		t.Fatalf("墓碑计数应为 1，实际 %d", m.Dead())
	}
	// 重复删除不应该再记一次
	if _, ok := m.Apply(Change{FRN: frn["docs"], Delete: true}, Rule{}); ok {
		t.Fatal("重复删除产生了多余指令")
	}
}

func TestMutableApplyCreate(t *testing.T) {
	m, frn := testTable(t)
	tab := m.Table()

	// 卷根下新建目录
	op, ok := m.Apply(Change{
		FRN: 300, ParentFRN: frn["root"], Name: "新目录",
		IsDir: true, Create: true,
	}, Rule{})
	if !ok {
		t.Fatal("建目录没被应用")
	}
	if op.PIdx != 0 || op.Name != "新目录" || Kind(op.Kind) != KindDir {
		t.Fatalf("指令不对: %+v", op)
	}
	if got := tab.PathOf(op.Idx); got != `C:\新目录` {
		t.Fatalf("新目录路径: %s", got)
	}
	// 同上，重复的 CREATE 不应重复追加
	if _, ok := m.Apply(Change{
		FRN: 300, ParentFRN: frn["root"], Name: "新目录", IsDir: true, Create: true,
	}, Rule{}); ok {
		t.Fatal("重复 CREATE 又追加了一条")
	}
}

func TestMutableApplyRename(t *testing.T) {
	m, frn := testTable(t)
	tab := m.Table()

	// 改名：旧名记录不算，新名记录带着同一个 FRN 过来
	if _, ok := m.Apply(Change{FRN: frn["report"], RenameOld: true, Name: "报告.txt"}, Rule{}); ok {
		t.Fatal("改名的旧记录不该产生指令")
	}
	op, ok := m.Apply(Change{
		FRN: frn["report"], ParentFRN: frn["docs"], Name: "总结.txt",
		RenameNew: true,
	}, Rule{})
	if !ok {
		t.Fatal("改名的新记录没被应用")
	}
	if op.Kill != 2 {
		t.Fatalf("改名应先失效旧条目 2，实际 Kill=%d", op.Kill)
	}
	// 旧下标 2 成了墓碑（旧名字不该再被搜到），新名字追加在 3
	if !tab.Dead(2) {
		t.Fatal("改名后旧条目没有失效")
	}
	if int(op.Idx) != 3 {
		t.Fatalf("改名后新条目应追加在 3，实际 %d", op.Idx)
	}
	if tab.Name(int(op.Idx)) != "总结.txt" {
		t.Fatalf("新条目名字不对: %s", tab.Name(int(op.Idx)))
	}
	if got := tab.PathOf(op.Idx); got != `C:\docs\总结.txt` {
		t.Fatalf("改名后路径: %s", got)
	}
}

func TestMutableApplyFilters(t *testing.T) {
	m, frn := testTable(t)
	rootFRN := frn["root"]
	// 父不在索引里 → 不收（和全量一致：可达性决定一切）
	if _, ok := m.Apply(Change{
		FRN: 400, ParentFRN: 999999, Name: "孤儿.txt", Create: true,
	}, Rule{}); ok {
		t.Fatal("父不在索引里还是被收了")
	}
	// reparse point 不跟进
	if _, ok := m.Apply(Change{
		FRN: 401, ParentFRN: rootFRN, Name: "链接", Create: true, IsReparse: true,
	}, Rule{}); ok {
		t.Fatal("reparse point 被收了")
	}
	// 隐藏文件默认不收
	if _, ok := m.Apply(Change{
		FRN: 402, ParentFRN: rootFRN, Name: "hid.dat", Create: true, IsHidden: true,
	}, Rule{}); ok {
		t.Fatal("隐藏文件被收了")
	}
	// 放开了就收
	if _, ok := m.Apply(Change{
		FRN: 403, ParentFRN: rootFRN, Name: "hid2.dat", Create: true, IsHidden: true,
	}, Rule{IncludeHidden: true}); !ok {
		t.Fatal("includeHidden 打开后还是被过滤")
	}
	// 隐藏目录一律收（隐藏目录里可能有正常文件）
	op, ok := m.Apply(Change{
		FRN: 404, ParentFRN: rootFRN, Name: "$Recycle", Create: true, IsDir: true, IsHidden: true,
	}, Rule{})
	if !ok {
		t.Fatal("隐藏目录被过滤了")
	}
	if Kind(op.Kind) != KindDir {
		t.Fatalf("隐藏目录的类型不对: %d", op.Kind)
	}
}

func TestDeltaRoundTripAndApply(t *testing.T) {
	m, frn := testTable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "index.delta")

	op1, ok1 := m.Apply(Change{
		FRN: 500, ParentFRN: frn["docs"], Name: "日志.txt", Create: true,
	}, Rule{})
	op2, ok2 := m.Apply(Change{FRN: frn["report"], Delete: true}, Rule{})
	if !ok1 || !ok2 {
		t.Fatal("两条变化没被应用")
	}
	d := &Delta{Base: 42, Seq: 1, Ops: []DeltaOp{op1, op2}}
	if err := WriteDelta(path, d); err != nil {
		t.Fatal(err)
	}
	back, err := ReadDelta(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Base != 42 || back.Seq != 1 || len(back.Ops) != 2 {
		t.Fatalf("往返丢了数据: %+v", back)
	}

	// 主进程侧：从同一张基态出发按顺序重放，下标必须和服务端完全一致
	base := &Table{Names: make([]byte, 0, 4096), Py: make(map[int32]uint64, 8)}
	baseM := NewMutable(base, NewFRNMap(8))
	baseM.Add(`C:\`, -1, KindDir, 0, 0, 5)
	baseM.Add("docs", 0, KindDir, 0, 0, 100)
	baseM.Add("报告.txt", 1, KindFile, 0, 0, 101)

	dead, ok := base.ApplyOps(back.Ops)
	if !ok {
		t.Fatal("重放失败（下标错位）")
	}
	if dead != 1 {
		t.Fatalf("重放后应产生 1 个墓碑，实际 %d", dead)
	}
	if base.Len() != 4 {
		t.Fatalf("重放后条目数应为 4，实际 %d", base.Len())
	}
	if got := base.PathOf(3); got != `C:\docs\日志.txt` {
		t.Fatalf("重放后路径不对: %s", got)
	}
	if base.Dead(2) != true {
		t.Fatal("重放后没把删除的条目标成墓碑")
	}
	if base.Dead(3) {
		t.Fatal("新增条目不该被判失效")
	}
}

func TestApplyOpsRejectsMisalignedIndex(t *testing.T) {
	// 少了一条基态（比如主进程载入的是旧一代快照）→ 下标对不上，必须报 ok=false
	base := &Table{Names: make([]byte, 0, 16), Py: make(map[int32]uint64, 2)}
	baseM := NewMutable(base, NewFRNMap(4))
	baseM.Add(`C:\`, -1, KindDir, 0, 0, 5)

	_, ok := base.ApplyOps([]DeltaOp{{
		Op: OpAdd, Idx: 2, PIdx: 0, Name: "x.txt", Kind: uint8(KindFile),
	}})
	if ok {
		t.Fatal("下标错位时不该继续应用")
	}
}

func TestFRNMap(t *testing.T) {
	m := NewFRNMap(4)
	for i := 0; i < 1000; i++ {
		m.Set(uint64(i+1)*7, int32(i))
	}
	for i := 0; i < 1000; i++ {
		idx, ok := m.Get(uint64(i+1) * 7)
		if !ok || idx != int32(i) {
			t.Fatalf("查不到 %d: idx=%d ok=%v", i, idx, ok)
		}
	}
	if _, ok := m.Get(0); ok {
		t.Fatal("FRN 0 不该存在")
	}
	if _, ok := m.Get(99999999); ok {
		t.Fatal("不存在的 FRN 查到了东西")
	}
	if m.Len() != 1000 {
		t.Fatalf("计数错误: %d", m.Len())
	}
}

func TestCompactTrigger(t *testing.T) {
	m, frn := testTable(t)
	m.Apply(Change{FRN: frn["report"], Delete: true}, Rule{})
	if m.Compact(10, 1<<20) {
		t.Fatal("墓碑才 1 个就要求整理")
	}
	if !m.Compact(1, 1<<20) {
		t.Fatal("墓碑到线却没要求整理")
	}
	if !m.Compact(10, 1) {
		t.Fatal("名字追加到线却没要求整理")
	}
}

// 冻结目录（Baozi 自己的 %LOCALAPPDATA%\Baozi）：条目在表里（搜得到），
// 但它的新增/删除一条都不能被应用 —— 否则它每 5 秒写一次 index.delta，
// 就会把自己喂成永不空闲的增量循环。
func TestFreezeKeepsEntriesButIgnoresChanges(t *testing.T) {
	tab := &Table{Names: make([]byte, 0, 4096), Py: make(map[int32]uint64, 8)}
	m := NewMutable(tab, NewFRNMap(8))
	root := m.Add(`C:\`, -1, KindDir, 0, 0, 5)
	users := m.Add("Users", root, KindDir, 0, 0, 10)
	admin := m.Add("Administrator", users, KindDir, 0, 0, 11)
	appData := m.Add("AppData", admin, KindDir, 0, 0, 12)
	local := m.Add("Local", appData, KindDir, 0, 0, 13)
	dataDir := m.Add("Baozi", local, KindDir, 0, 0, 14)
	logFRN := uint64(15)
	log := m.Add("baozi.log", dataDir, KindFile, 0, 0, logFRN)

	frozen := Rule{Freeze: []string{`AppData\Local\Baozi`}}

	// 它本来就在表里：搜得到，路径也对。
	if got := tab.PathOf(log); got != `C:\Users\Administrator\AppData\Local\Baozi\baozi.log` {
		t.Fatalf("冻结目录里的条目应当在表里，实际路径 %q", got)
	}
	// 目录里新建文件：不收。
	if _, ok := m.Apply(Change{
		FRN: 20, ParentFRN: 14, Name: "index.delta", Create: true,
	}, frozen); ok {
		t.Fatal("冻结目录里的新增被应用了（会自己喂自己）")
	}
	// 目录里删文件：不收（写 .tmp 再改名就是这一对记录）。
	if _, ok := m.Apply(Change{FRN: logFRN, Delete: true}, frozen); ok {
		t.Fatal("冻结目录里的删除被应用了")
	}
	if tab.KindOf(log) == KindDeleted {
		t.Fatal("冻结目录里的条目被标成墓碑了，用户就搜不到自己的日志")
	}
	// 冻结之外的目录照常应用，别把整套增量弄停了。
	if _, ok := m.Apply(Change{
		FRN: 21, ParentFRN: 5, Name: "other.txt", Create: true,
	}, frozen); !ok {
		t.Fatal("冻结规则误伤了普通目录")
	}
}
