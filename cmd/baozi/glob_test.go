package main

import (
	"testing"

	"baozi/internal/index"
	"github.com/egoist/mygo/ui"
)

type globEntry struct {
	name string
	dir  bool
}

// globIndex 造一张平铺的索引表：第一个条目是盘根，其余挂在根下。
// 名字在 arena 里顺序拼接，偏移现算，测试里不必手数。
func globIndex(entries ...globEntry) *index.Table {
	var names []byte
	nodes := make([]index.Node, 0, len(entries))
	for i, e := range entries {
		kind, parent := index.KindFile, int32(0)
		if i == 0 {
			kind, parent = index.KindDir, -1
		} else if e.dir {
			kind = index.KindDir
		}
		nodes = append(nodes, index.Node{
			Parent:  parent,
			NameOff: uint32(len(names)),
			NameLen: uint16(len(e.name)),
			Kind:    kind,
		})
		names = append(names, e.name...)
	}
	return &index.Table{Nodes: nodes, Names: names}
}

// `*.pdf` 在普通模式下必须能搜出 pdf 文件 —— 以前 `*` 被当成普通字符，
// 子串预筛要求文件名里真有个星号，候选一条都进不来，结果就是"搜不到"。
func TestGlobSearchesFiles(t *testing.T) {
	a := testApp(t)
	a.cat.Store(globIndex(
		globEntry{`C:\`, true},
		globEntry{name: "a.pdf"},
		globEntry{name: "b.PDF"}, // 资源管理器里 *.PDF 和 *.pdf 是一回事
		globEntry{name: "c.txt"},
		globEntry{name: "2026年报.pdf"},
		globEntry{name: "pdf", dir: true}, // 名字就叫 pdf 的目录：不是 *.pdf
		globEntry{name: "x.pdf.bak"},      // 锚定整名，不是"含 .pdf"
	))

	for _, q := range []string{`*.pdf`, `*.PDF`, `*.Pdf`} {
		a.query = q
		found := names(a.searchAll(q))
		if len(found) != 3 {
			t.Fatalf("%q 期望 3 条（a.pdf / b.PDF / 2026年报.pdf），实际 %d 条：%v", q, len(found), found)
		}
		hit := map[string]bool{}
		for _, n := range found {
			hit[n] = true
		}
		for _, want := range []string{"a.pdf", "b.PDF", "2026年报.pdf"} {
			if !hit[want] {
				t.Fatalf("%q 没命中 %q（实际 %v）", q, want, found)
			}
		}
		for _, bad := range []string{"c.txt", "pdf", "x.pdf.bak"} {
			if hit[bad] {
				t.Fatalf("%q 不该命中 %q（实际 %v）", q, bad, found)
			}
		}
	}
}

// 通配符的其他写法：前后缀组合、`?`、字符集。
func TestGlobPatterns(t *testing.T) {
	a := testApp(t)
	a.cat.Store(globIndex(
		globEntry{`C:\`, true},
		globEntry{name: "2026-report.log"},
		globEntry{name: "2027-report.log"},
		globEntry{name: "笔记1.md"},
		globEntry{name: "笔记12.md"},
		globEntry{name: "a.txt"},
		globEntry{name: "b.txt"},
	))

	cases := []struct {
		q    string
		want int
	}{
		{`2026*.log`, 1},
		{`*report.log`, 2},
		{`笔记?.md`, 1},
		{`[ab]*.txt`, 2},
		{`[!a]*.txt`, 1},
	}
	for _, c := range cases {
		a.query = c.q
		if got := a.searchAll(c.q); len(got) != c.want {
			t.Fatalf("%q 期望 %d 条，实际 %d 条（%v）", c.q, c.want, len(got), names(got))
		}
	}

	// 方括号单独出现**不**算通配符意图：文件名里带 [草稿] 这种前缀太常见，
	// 按字面搜才对。这里没有这个名字，所以是 0 条 —— 但关键在于它没被当成字符集。
	a.query = `[ab]`
	if got := a.searchAll(`[ab]`); len(got) != 0 {
		t.Fatalf("裸方括号不该按字符集匹配，实际 %v", names(got))
	}
}

// `.*` 正则按钮优先：开着正则时 `*` 就是正则的重复号，通配符不该抢这条解释权。
// （`*` 开头那条正则不合法，所以正确表现是"报错"，而不是"搜出所有 pdf"。）
func TestGlobYieldsToRegexMode(t *testing.T) {
	a := testApp(t)
	a.cat.Store(globIndex(
		globEntry{`C:\`, true},
		globEntry{name: "a.pdf"},
	))

	a.regexOn = true
	a.query = `*.pdf`
	if got := a.searchAll(`*.pdf`); len(got) != 0 {
		t.Fatalf("正则模式下 %q 非法，不该有结果，实际 %v", `*.pdf`, names(got))
	}
	if a.patternErr == "" {
		t.Fatal("非法正则没有记录错误")
	}

	// 用户写的合法正则照旧是正则：`.*\.pdf$` 命中 a.pdf，没有被翻成通配符
	a.query = `.*\.pdf$`
	if got := a.searchAll(`.*\.pdf$`); len(got) != 1 || got[0].Name != "a.pdf" {
		t.Fatalf("正则模式没能命中 a.pdf，实际 %v", names(got))
	}
}

// 不带通配符的查询仍走模糊搜索：`pdf` 该把名字就叫 pdf 的目录也带出来，
// 不能被悄悄改道成"只按模式精确匹配"。
func TestPlainQueryStaysFuzzy(t *testing.T) {
	a := testApp(t)
	a.cat.Store(globIndex(
		globEntry{`C:\`, true},
		globEntry{name: "pdf", dir: true},
		globEntry{name: "a.pdf"},
	))

	a.query = "pdf"
	if got := a.searchAll("pdf"); len(got) != 2 {
		t.Fatalf("普通模式搜 pdf 应当同时命中目录和文件，实际 %v", names(got))
	}
}

// 空结果时的提示要分得清：正则模式、通配模式各有自己的说法。
// 都写成"没有匹配项"的话，用户会以为功能坏了。
func TestGlobEmptyMessage(t *testing.T) {
	a := testApp(t)
	a.query = `a?.log`
	if got := a.searchAll(`a?.log`); len(got) != 0 {
		t.Fatalf("空索引上不该有结果，实际 %v", names(got))
	}
	if a.patternErr != "" {
		t.Fatalf("合法通配模式不该记录错误：%q", a.patternErr)
	}

	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	if !tr.HasText("没有文件名匹配这个通配模式") {
		t.Fatal("通配模式空结果没有专门的提示")
	}
}
