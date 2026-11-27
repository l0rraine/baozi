package search

import "testing"

// 通配符的基本语义：整名锚定、`*`/`?`/`[...]`、不区分大小写。
func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{`*.pdf`, "a.pdf", true},
		{`*.pdf`, "a.PDF", true}, // 资源管理器里 *.PDF 和 *.pdf 是一回事
		{`*.pdf`, "报告.pdf", true},
		{`*.pdf`, "a.txt", false},
		// 锚定整名：`*.pdf` 是"以 .pdf 结尾"，不是"含 .pdf"
		{`*.pdf`, "a.pdf.bak", false},
		{`2026*.log`, "2026-report.log", true},
		{`2026*.log`, "2027-report.log", false},
		{`笔记?.md`, "笔记1.md", true},
		{`笔记?.md`, "笔记12.md", false},
		{`*`, "anything.txt", true},
		// 字符集：范围、取反、Windows 的 `!` 和正则的 `^` 都认
		{`[ab].txt`, "a.txt", true},
		{`[a-c]1.txt`, "b1.txt", true},
		{`[a-c]1.txt`, "d1.txt", false},
		{`[!a].txt`, "b.txt", true},
		{`[!a].txt`, "a.txt", false},
		{`[^a].txt`, "a.txt", false},
		// 转义：名字里真有星号的文件
		{`\*.pdf`, "*.pdf", true},
		{`\*.pdf`, "a.pdf", false},
		// 没闭合的 `[` 当普通字符，别让一个手滑整条失效
		{`[abc`, "[abc", true},
		{`[abc`, "abc", false},
	}
	for _, c := range cases {
		rx, err := CompileGlob(c.pattern)
		if err != nil {
			t.Fatalf("%q 编译失败：%v", c.pattern, err)
		}
		if got := rx.Match([]byte(c.name)); got != c.want {
			t.Fatalf("%q 匹配 %q = %v，期望 %v", c.pattern, c.name, got, c.want)
		}
	}
}

// 空模式要报错，不能悄悄命中一切。
func TestGlobEmptyPattern(t *testing.T) {
	if _, err := CompileGlob("   "); err == nil {
		t.Fatal("空通配符模式应当返回错误")
	}
}

// 预筛只能比真匹配更宽松：Maybe 说"不可能"才算数，绝不能在真能命中的名字上返回 false。
// 这条比上面那个逐例测试更要紧 —— 预筛算错的表现是"有些文件死活搜不出来"，
// 比多扫几百万条贵得多。
func TestGlobScreenNeverTooStrict(t *testing.T) {
	pats := []string{
		`*.pdf`,
		`2026*.log`,
		`*.pdf`, `*报告*`, `*.tar.gz`, `a?c*`, `[ab]*.txt`,
		`*2026*年报*`, `**`, `?`,
	}
	names := []string{
		"a.pdf", "A.PDF", "报告.pdf", "2026-report.log", "报告2026年报.docx",
		"a.tar.gz", "abc.txt", "b.txt", "x", "中文名.pdf", "结论 报告.pdf",
	}
	for _, p := range pats {
		rx, err := CompileGlob(p)
		if err != nil {
			t.Fatalf("%q 编译失败：%v", p, err)
		}
		for _, n := range names {
			if !rx.Match([]byte(n)) {
				continue
			}
			if !rx.Maybe([]byte(n)) {
				t.Fatalf("%q 命中了 %q，但预筛把它挡掉了", p, n)
			}
		}
	}
}

// 预筛要真的省事：`*.pdf` 求出来的字面是 ".pdf"，不是整条模式。
func TestGlobScreenUsesLiteral(t *testing.T) {
	rx, err := CompileGlob(`*.pdf`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rx.screen) != 1 || len(rx.screen[0]) != 1 || rx.screen[0][0] != ".pdf" {
		t.Fatalf("*.pdf 的预筛条件 = %v，期望 [[.pdf]]", rx.screen)
	}
	// 多个必现字面是 AND：`2026*.log` 两个条件都要过
	rx, err = CompileGlob(`2026*.log`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rx.screen) != 1 || len(rx.screen[0]) != 2 {
		t.Fatalf("2026*.log 的预筛条件 = %v，期望两组字面", rx.screen)
	}
	if rx.Maybe([]byte("2026-a.txt")) {
		t.Fatal("只命中 2026、不含 .log 的名字不该放过")
	}
}

// 纯中文模式求不出安全字面（(?i) 会折 Unicode），就**不预筛**，
// 但匹配本身必须照常工作。
func TestGlobChineseOnlyPatternMatches(t *testing.T) {
	rx, err := CompileGlob(`*年报*`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rx.screen) != 0 {
		t.Fatalf("纯中文模式不该有预筛条件，实际 %v", rx.screen)
	}
	if !rx.Match([]byte("2026年报.pdf")) || !rx.Maybe([]byte("2026年报.pdf")) {
		t.Fatal("纯中文模式应当命中 2026年报.pdf")
	}
	if rx.Match([]byte("2026-季报.pdf")) {
		t.Fatal("不该命中不含年报的名字")
	}
}

// HasGlob 只认 `*` 和 `?`：方括号单独出现不算通配符意图。
func TestHasGlob(t *testing.T) {
	cases := map[string]bool{
		`*.pdf`:   true,
		`a?c`:     true,
		`pdf`:     false,
		`[草稿]`:    false,
		`2026`:    false,
		`a b`:     false,
		`*`:       true,
		`配置*.ini`: true,
	}
	for q, want := range cases {
		if got := HasGlob(q); got != want {
			t.Fatalf("HasGlob(%q) = %v，期望 %v", q, got, want)
		}
	}
}
