package index

import (
	"strings"

	"baozi/internal/pinyin"
)

// 这个文件是"索引能不能实时更新"的关键。
//
// 原来的设计是：索引是一张不可变快照（定长 Node + 名字 arena + 父节点存下标），
// 增量只能当"什么时候值得重扫"的触发器。代价是重建一次 15 秒，而重建频率直接
// 决定常驻 CPU。
//
// 现在改成可增删，但要绕开"下标不能乱"这个限制：**只追加、不压缩**。
//   - 新增条目 append 到 Nodes/Names 末尾，已有下标全都不动；
//   - 删除只把 Kind 改成墓碑，槽位留着（删目录时子条目不产生 USN 记录，
//     父子链必须留着才能让子条目跟着失效）；
//   - 按 FRN 找条目靠一张开放寻址哈希（只在服务进程内存里，不落盘）。
//
// 攒够墓碑或 arena 涨太多，再走一次全量重建把它压回去。这就是 Everything 的做法。

// Change 是一条目录变化。由 usn 包把 USN_RECORD 翻译成这个结构，
// 这样 index 不用认识 USN 的位标志，也不会和 usn 互相依赖。
type Change struct {
	FRN       uint64
	ParentFRN uint64
	Name      string
	IsDir     bool
	IsHidden  bool
	IsReparse bool
	Delete    bool
	Create    bool
	RenameNew bool
	RenameOld bool
}

// Rule 是应用增量时要遵守的收录规则（和全量建索引必须一致，
// 否则同一次变化在两条路径上会给出不同结果）。
type Rule struct {
	Excludes []string
	// Freeze 是"收录但不跟增量"的目录（见 config.Index.Frozen）：
	// 全量建索引时它们照常进来，但这里来的变化一律丢弃。
	//
	// 为什么不干脆排除：排除了就搜不到用户自己的 config.json / baozi.log。
	// 而不冻结又不行——Baozi 每 5 秒写一次索引增量、每次唤出写一行日志，
	// 那些 USN 记录会被当成"用户动了文件"，索引再攒一条增量再写盘，5 秒一轮。
	Freeze        []string
	IncludeHidden bool
}

// 主进程看到的增量指令。注意它带的是**下标**而不是 FRN：
// 服务端和主进程从同一份 index.bin 出发、按同一顺序追加，
// 下标天然对齐，主进程不必再维护一张 FRN 表（省 25MB）。
const (
	OpAdd    = "add"
	OpDelete = "del"
)

// DeltaOp 是一条增量指令。
// Kill 表示"新增前先把这个旧条目失效"（改名），0 表示没有——
// 下标 0 永远是卷根（Parent == -1），它不可能被改名，所以用 0 当空值是安全的。
type DeltaOp struct {
	Op   string `json:"op"`
	Idx  int32  `json:"idx"`            // OpDelete：要标墓碑的条目下标
	Kill int32  `json:"kill,omitempty"` // OpAdd：新增前先失效的旧条目（改名）
	PIdx int32  `json:"pidx,omitempty"` // OpAdd：父节点下标
	Name string `json:"name,omitempty"`
	Kind uint8  `json:"kind,omitempty"`
	Py   string `json:"py,omitempty"` // 拼音首字母串，服务端算好带过来
}

// ---------------------------------------------------------------- FRN → 下标

// FRNMap 是 FRN 到条目下标的开放寻址哈希。
//
// 为什么不用 map[uint64]int32：179 万条时 Go 的 map 每条约 40~50 字节开销
// （约 80MB）。
//
// 为什么不用"按 FRN 排序的数组 + 二分"：有序数组插入要移动后半段
// （平均 3.6MB memmove），增量是热路径，不能这么干。
//
// 为什么不落盘：服务每次启动都会做一次全量（顺便把这张表建起来），
// 没必要为它把索引文件再撑大三四十兆。
//
// 布局用**两张并行数组**而不是 []struct{uint64; int32}：后者因为 8 字节对齐
// 会撑到 16 字节一槽，而键和值分别是 8 / 4 字节，拆开就只占 12 字节。
// 容量按实际条数算（取模，不是 2 的幂掩码），179 万条时约 34MB——
// 用 2 的幂掩码会被迫翻到 419 万槽，多占 30MB。
type FRNMap struct {
	keys  []uint64 // 0 表示空槽（FRN 经过掩码后不可能是 0）
	vals  []int32
	used  int
	limit int // 装载上限，到这个数就扩容
}

// NewFRNMap 按预估条数分配。hint 只影响初始容量，估大了不会浪费太多（取模不必取整到 2 的幂）。
func NewFRNMap(hint int) *FRNMap {
	if hint < 256 {
		hint = 256
	}
	m := &FRNMap{}
	m.alloc(hint * 5 / 4) // 装载率 0.8
	return m
}

func (m *FRNMap) alloc(size int) {
	if size < 256 {
		size = 256
	}
	m.keys = make([]uint64, size)
	m.vals = make([]int32, size)
	m.used = 0
	m.limit = size * 4 / 5
}

// Len 返回已有条目数。
func (m *FRNMap) Len() int {
	if m == nil {
		return 0
	}
	return m.used
}

// mix64 是 murmur3 的收尾混合，比 FNV 在这个规模上分布更均匀。
func mix64(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

// Set 写入/覆盖一个 FRN 的下标。
func (m *FRNMap) Set(frn uint64, idx int32) {
	if m == nil || frn == 0 {
		return
	}
	if m.used >= m.limit {
		m.grow()
	}
	m.put(frn, idx)
}

func (m *FRNMap) put(frn uint64, idx int32) {
	size := uint64(len(m.keys))
	i := mix64(frn) % size
	for {
		if m.keys[i] == 0 {
			m.keys[i], m.vals[i] = frn, idx
			m.used++
			return
		}
		if m.keys[i] == frn {
			m.vals[i] = idx
			return
		}
		i++
		if i == size {
			i = 0
		}
	}
}

// Get 查 FRN 对应的条目下标。
func (m *FRNMap) Get(frn uint64) (int32, bool) {
	if m == nil || frn == 0 || len(m.keys) == 0 {
		return -1, false
	}
	size := uint64(len(m.keys))
	i := mix64(frn) % size
	for n := 0; n < len(m.keys); n++ {
		if m.keys[i] == 0 {
			return -1, false
		}
		if m.keys[i] == frn {
			return m.vals[i], true
		}
		i++
		if i == size {
			i = 0
		}
	}
	return -1, false
}

func (m *FRNMap) grow() {
	oldKeys, oldVals := m.keys, m.vals
	m.alloc(len(oldKeys) * 2)
	for i, k := range oldKeys {
		if k != 0 {
			m.put(k, oldVals[i])
		}
	}
}

// ---------------------------------------------------------------- Mutable

// Mutable 是服务进程里实时维护的索引。
// 全量重建后拿到 *Table（Nodes 是普通切片，可 append），之后按 USN 增量改它。
type Mutable struct {
	t *Table
	// frn 只放"基座里没有的"那一小撮（重建之后新增/改名过的条目）。
	// 基座那 177 万条在 base 里（映射文件，零堆占用，见 frnfile.go）。
	frn  *FRNMap
	base *FRNFile
	dead int // 墓碑条数
	// names0 是建立映射时名字 arena 的长度，用来估算"追加了多少垃圾"。
	names0 int
}

// NewMutable 把一张刚建好的快照包装成可增量维护的形式（堆上的 FRN 表）。
func NewMutable(t *Table, frn *FRNMap) *Mutable {
	if t.Py == nil {
		t.Py = make(map[int32]uint64, 1<<13)
	}
	if frn == nil {
		frn = NewFRNMap(t.Len())
	}
	return &Mutable{t: t, frn: frn, names0: 0}
}

// NewMutableBase 用**映射进来的** FRN 表当基座：增量新增的条目走堆上的小表，
// 基座那 177 万条直接查文件。服务端稳态用这个，把原来那 30MB 堆换成文件页。
func NewMutableBase(t *Table, base *FRNFile) *Mutable {
	m := &Mutable{t: t, base: base, names0: 0}
	if t.Py == nil {
		t.Py = make(map[int32]uint64, 1<<13)
	}
	// 增量表按"预计几百条"起步，重建周期内够用；超了会自己长。
	m.frn = NewFRNMap(1024)
	return m
}

// FRNCount 返回基座 + 增量两部分的条目数（日志/诊断用）。
func (m *Mutable) FRNCount() int {
	return m.FRNBaseLen() + m.FRNOvLen()
}

// FRNBaseLen 返回映射基座里的条目数。
func (m *Mutable) FRNBaseLen() int {
	if m == nil {
		return 0
	}
	return m.base.Len()
}

// FRNOvLen 返回堆上增量表里的条目数。
func (m *Mutable) FRNOvLen() int {
	if m == nil {
		return 0
	}
	return m.frn.Len()
}

// LookupFRN 是 lookupFRN 的导出外壳，只给诊断工具用（tools/frnreplay）。
func (m *Mutable) LookupFRN(frn uint64) (int32, bool) { return m.lookupFRN(frn) }

// lookupFRN 先查增量（改名会把老条目指向新下标，所以增量必须优先），再查基座。
func (m *Mutable) lookupFRN(frn uint64) (int32, bool) {
	if m == nil || frn == 0 {
		return -1, false
	}
	if m.frn != nil {
		if idx, ok := m.frn.Get(frn); ok {
			return idx, true
		}
	}
	if m.base != nil {
		return m.base.Get(frn)
	}
	return -1, false
}

// setFRN 只往增量表里记（基座是只读映射，改不了也不该改）。
func (m *Mutable) setFRN(frn uint64, idx int32) {
	if m == nil || frn == 0 {
		return
	}
	if m.frn == nil {
		m.frn = NewFRNMap(1024)
	}
	m.frn.Set(frn, idx)
}

// Close 解开基座映射。换代次时由服务端调用（旧表可能还有搜索线程在用，
// 但服务端这边只在自己手里握着，换完就没人查了）。
func (m *Mutable) Close() {
	if m == nil || m.base == nil {
		return
	}
	m.base.Close()
	m.base = nil
}

// Table 返回当前（可继续被修改的）表。
func (m *Mutable) Table() *Table { return m.t }

// Dead 返回墓碑条数。
func (m *Mutable) Dead() int { return len(m.t.dead) }

// NamesOverhead 返回名字 arena 里追加出来的字节数（相对上次全量）。
func (m *Mutable) NamesOverhead() int { return len(m.t.ovNames) - m.names0 }

// Add 追加一个条目并登记 FRN，返回下标。
func (m *Mutable) Add(name string, parent int32, kind Kind, sizeKB, modSec int32, frn uint64) int32 {
	idx := m.t.appendOverlay(name, parent, kind, sizeKB, modSec, pinyin.Initials(name))
	if frn != 0 {
		m.setFRN(frn, idx)
	}
	return idx
}

// Kill 把条目标成墓碑。
func (m *Mutable) Kill(idx int32) {
	if idx < 0 || int(idx) >= m.t.Len() {
		return
	}
	if m.t.KindOf(idx) == KindDeleted {
		return
	}
	m.t.markDead(idx)
	m.dead++
}

// Apply 把一条变化应用到索引，返回给主进程的增量指令（第二个返回值表示有没有变化）。
//
// 规则必须和全量建索引一致，否则同一次变化在两条路径上会给出不同结果：
// 隐藏文件不收（隐藏目录照收）、reparse point 不跟进、排除规则照跑、
// 父节点不在索引里就不收（父目录被排除或被删，整棵子树都不可达）。
func (m *Mutable) Apply(c Change, rule Rule) (DeltaOp, bool) {
	if m.t == nil || c.FRN == 0 {
		return DeltaOp{}, false
	}
	// 改名会出两条记录（旧名/新名），旧名那条什么都不做：
	// 新名那条带着同一个 FRN 过来，那时才覆盖旧条目。
	if c.RenameOld && !c.RenameNew && !c.Delete {
		return DeltaOp{}, false
	}
	if c.Delete {
		idx, ok := m.lookupFRN(c.FRN)
		if !ok {
			return DeltaOp{}, false
		}
		if m.t.KindOf(idx) == KindDeleted {
			return DeltaOp{}, false
		}
		// 冻结目录里的删除也不收（理由见 Rule.Freeze）：Baozi 自己写 index.delta
		// 就是"写 .tmp 再改名"，那两条记录要是收下来，每 5 秒就攒一条增量。
		// 代价是真删掉的文件还会留在索引里一条旧记录，下一次全量重建自然清掉。
		if len(rule.Freeze) > 0 {
			if p := m.t.PathOf(idx); p != "" && ExcludedPathOnly(rule.Freeze, p) {
				return DeltaOp{}, false
			}
		}
		m.Kill(idx)
		return DeltaOp{Op: OpDelete, Idx: idx}, true
	}
	if (!c.Create && !c.RenameNew) || c.Name == "" {
		return DeltaOp{}, false
	}
	if c.IsReparse {
		return DeltaOp{}, false
	}

	parent, ok := m.lookupFRN(c.ParentFRN)
	if !ok {
		return DeltaOp{}, false
	}
	if m.t.KindOf(parent) == KindDeleted {
		return DeltaOp{}, false
	}

	kind := KindFile
	if c.IsDir {
		kind = KindDir
	}
	if kind == KindFile && c.IsHidden && !rule.IncludeHidden {
		return DeltaOp{}, false
	}
	if ExcludedNameOnly(rule.Excludes, c.Name) {
		return DeltaOp{}, false
	}
	// 路径级规则要完整路径，只有真配了带分隔符的规则才付这个代价。
	// 排除和冻结合起来判一次：两者都可能是路径规则，只拼一次路径。
	if hasPathRule(rule.Excludes) || hasPathRule(rule.Freeze) {
		if dir := m.t.PathOf(parent); dir != "" {
			p := dir + `\` + c.Name
			if ExcludedPathOnly(rule.Freeze, p) || ExcludedPathOnly(rule.Excludes, p) {
				return DeltaOp{}, false
			}
		}
	}

	// 同一个 FRN 已经有活条目：要么是重复记录（同名同父，忽略），
	// 要么是改名（旧名字必须失效，否则两个名字都能搜到）。
	var kill int32
	if old, ok := m.lookupFRN(c.FRN); ok && int(old) < m.t.Len() {
		if m.t.KindOf(old) != KindDeleted {
			if n := m.t.At(old); n != nil && n.Parent == parent && string(m.t.NameBytes(int(old))) == c.Name {
				return DeltaOp{}, false
			}
			m.Kill(old)
			kill = old
		}
	}

	idx := m.Add(c.Name, parent, kind, 0, 0, c.FRN)
	op := DeltaOp{
		Op:   OpAdd,
		Idx:  idx,
		Kill: kill,
		PIdx: parent,
		Name: c.Name,
		Kind: uint8(kind),
	}
	if off, n := UnpackPy(m.t.Py[idx]); n > 0 {
		op.Py = string(m.t.Names[off : off+uint32(n)])
	}
	return op, true
}

// hasPathRule 判断排除规则里有没有"路径片段"那类（带分隔符的）。
func hasPathRule(rules []string) bool {
	for _, r := range rules {
		if strings.ContainsAny(strings.TrimSpace(r), `\/`) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 整理

// Compact 判断是不是该做一次全量重建把墓碑和 arena 垃圾收掉。
// 两个指标：墓碑数（搜索时要多跳几次指针）和名字 arena 的追加量（纯浪费内存）。
func (m *Mutable) Compact(deadLimit, namesLimit int) bool {
	if deadLimit <= 0 {
		deadLimit = 20000
	}
	if namesLimit <= 0 {
		namesLimit = 8 << 20
	}
	return len(m.t.dead) >= deadLimit || m.NamesOverhead() >= namesLimit
}
