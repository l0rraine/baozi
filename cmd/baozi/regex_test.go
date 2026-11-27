package main

import (
	"testing"

	"baozi/internal/index"
	"baozi/internal/search"

	"github.com/egoist/mygo/ui"
)

// 正则模式下拼音与模糊一律不参与：dbgz 是「豆包工作」的拼音首字母，
// 普通模式能搜到，正则模式只看字面，必须搜不到。
func TestRegexModeIgnoresPinyin(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)

	// 先确认普通模式的基线成立，否则这个测试等于没测
	if got := a.searchAll("dbgz"); len(got) == 0 {
		t.Fatal("普通模式应当能靠拼音搜到「豆包工作」")
	}

	a.regexOn = true
	a.query = "dbgz"
	if got := a.searchAll("dbgz"); len(got) != 0 {
		t.Fatalf("正则模式不该靠拼音命中，实际 %d 条：%+v", len(got), got[0].Name)
	}

	// 字面命中照旧：逗包工作里的「豆包」是文件名字面
	a.query = "豆包"
	got := a.searchAll("豆包")
	if len(got) == 0 {
		t.Fatal("正则模式按名字字面应当能命中「豆包工作」")
	}
}

// 模糊匹配（多 token 拆词、词首/子序列这些打分档位）在正则模式下也必须关掉：
// 普通模式里 "doubao exe" 是两个 token，空格被当分隔符；
// 正则模式下空格就是真字面，互相就分不开了。
func TestRegexModeIgnoresFuzzyTokens(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)

	if got := a.searchAll("doubao exe"); len(got) == 0 {
		t.Fatal("普通模式应当把 doubao exe 当两个 token 命中 doubao.exe")
	}

	a.regexOn = true
	a.query = "doubao exe"
	if got := a.searchAll("doubao exe"); len(got) != 0 {
		t.Fatalf("正则模式里空格就是字面，不该命中，实际 %d 条", len(got))
	}
}

// 正则本身要真的按用户写的模式走：锚定、扩展名、分支都要生效。
func TestRegexModeMatchesPattern(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)
	a.regexOn = true

	cases := []struct {
		q    string
		want int // 命中的索引/应用/命令条数
	}{
		{`^doubao\.exe$`, 1}, // 锚定的整名匹配
		{`^doubao`, 1},
		{`^doubaooo$`, 0},
		{`\.exe$`, 1},
		{`豆包$`, 0},  // 「豆包工作」不以「豆包」结尾
		{`工作$`, 2},  // 目录「豆包工作」+ 同名应用「豆包工作」
		{`^豆包$`, 0}, // 锚定整名就不该命中「豆包工作」
	}
	for _, c := range cases {
		a.query = c.q
		got := a.searchAll(c.q)
		if len(got) != c.want {
			t.Fatalf("%q: 期望 %d 条，实际 %d 条（%v）", c.q, c.want, len(got), names(got))
		}
	}
}

func names(rs []search.Result) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

// 正则写错时必须把语法错误说出来，而不是安静地给空结果。
func TestRegexInvalidPatternReportsError(t *testing.T) {
	a := testApp(t)
	a.regexOn = true
	a.query = "("
	if got := a.searchAll("("); len(got) != 0 {
		t.Fatalf("非法正则不该有结果，实际 %d 条", len(got))
	}
	if a.patternErr == "" {
		t.Fatal("非法正则没有记录语法错误")
	}

	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	if !tr.HasText("正则无效") {
		t.Fatal("结果区没有提示正则无效")
	}
}

// 顶上那个 ".*" 按钮：点一下开正则，再点一下关。
func TestRegexButtonToggles(t *testing.T) {
	a := testApp(t)
	a.query = ""
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	if !tr.HasText(".*") {
		t.Fatal("搜索框右边没有 .* 按钮")
	}
	rect, ok := tr.Find(".*")
	if !ok {
		t.Fatal("找不到 .* 按钮的位置")
	}
	tr.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tr.Frame()
	if !a.regexOn {
		t.Fatal("点了 .* 没有进入正则模式")
	}
	// 进入正则模式后底栏状态要说明现在按什么搜，用户才知道结果为什么变了
	if !tr.HasText("正则模式") {
		t.Fatal("进入正则模式后底栏没有说明状态")
	}
	dumpPreview(t, tr) // BAOZI_PREVIEW 指向哪就截在开着正则的这一帧

	rect, ok = tr.Find(".*")
	if !ok {
		t.Fatal("正则模式下 .* 按钮不见了")
	}
	tr.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tr.Frame()
	if a.regexOn {
		t.Fatal("再点一次 .* 应该回到普通搜索")
	}
}

// 正则模式只匹配**文件名**，不匹配路径：普通模式里 "含 \ 或 /" 会切到路径匹配，
// 但正则里反斜杠到处都是（\.log$），那个判断不能沿用到这条路上。
func TestRegexModeMatchesNameNotPath(t *testing.T) {
	a := testApp(t)
	tab := &index.Table{
		Nodes: []index.Node{
			{Parent: -1, Kind: index.KindDir, NameOff: 0, NameLen: 3},
			{Parent: 0, Kind: index.KindDir, NameOff: 3, NameLen: 7},
			{Parent: 1, Kind: index.KindFile, NameOff: 10, NameLen: 8},
		},
		// 目录 stretch，文件 note.txt：路径里有 "stretch"，文件名字里没有
		Names: []byte(`C:\stretch` + `note.txt`),
	}
	a.cat.Store(tab)

	a.regexOn = true
	// 「stretch」只能命中名字就叫 stretch 的那条目录，
	// 绝不能因为路径里含 stretch 就把 note.txt 也带出来。
	a.query = `stretch`
	if got := a.searchAll(`stretch`); len(got) != 1 || got[0].Name != "stretch" {
		t.Fatalf("正则模式不该按路径命中，实际 %d 条（%v）", len(got), names(got))
	}
	a.query = `^note\.txt$`
	if got := a.searchAll(`^note\.txt$`); len(got) != 1 {
		t.Fatalf("按文件名应当命中 1 条，实际 %d 条", len(got))
	}
}

// 正则模式的分档：命中就是精确命中，准确度恒为 1.0，排序交给频次与类型。
func TestRegexAccuracyIsFixed(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)
	a.regexOn = true
	a.query = `doubao\.exe`
	got := a.searchAll(`doubao\.exe`)
	if len(got) == 0 {
		t.Fatal("没命中 doubao.exe")
	}
	for _, r := range got {
		if r.Accuracy != search.RegexAccuracy {
			t.Fatalf("%q 的准确度 = %v，正则模式应当固定为 %v", r.Name, r.Accuracy, search.RegexAccuracy)
		}
	}
}
