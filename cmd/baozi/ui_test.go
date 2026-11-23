package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"baozi/internal/config"
	"baozi/internal/history"
	"baozi/internal/index"
	"baozi/internal/search"
	"baozi/internal/share"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// stubAllIcons 让测试里的图标不走 Shell：Tester 没有 native 消息循环，
// Shell.FileIcon 内部是 onMainValue，会一直阻塞到超时。
// 打开 offline 后应用图标直接返回 nil，视图退回通用线框图标。
func stubAllIcons(a *application) {
	a.offline = true
}

// 用 mygo 的无窗口 Tester 渲染一帧：这类错误（控件 API 用法不对、
// 布局在某个分支里 nil 解引用）只有真的构建这一帧才会暴露。
func testApp(t *testing.T) *application {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Index.Roots = []string{dir}
	// BAOZI_PANEL_W / BAOZI_PANEL_H 用来在预览里试"面板调大之后长什么样"，
	// 顺手也验证了列数、方向键换算都跟着宽度走。不设就是默认 720×480。
	if v := os.Getenv("BAOZI_PANEL_W"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Panel.Width = n
		}
	}
	if v := os.Getenv("BAOZI_PANEL_H"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Panel.Height = n
		}
	}
	h, err := history.Open(filepath.Join(dir, "history.json"), 50, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	// 视图测试里不设自定义字体：解析字体族要回 native 主线程，ui.Tester 没有主循环，
	// 设了测试会永久卡住（和 Shell.* / mygo.App.* 在测试里的毛病同源）。
	fontOn = false
	return &application{
		cfg:     cfg,
		cfgPath: filepath.Join(dir, "config.json"),
		dataDir: dir,
		hist:    h,
		icons:   map[string]*ui.Bitmap{},
		status:  "测试",
		// 真机上 a.scale 是在 ready() 里问系统拿的；测试里用 BAOZI_SCALE 模拟，
		// 好在高 DPI 下截实拍（不设就是 1，即 100% 缩放）。
		scale: testScale(),
	}
}

// testScale 读 BAOZI_SCALE（如 1.25 / 1.5）来模拟显示器缩放，默认 1。
func testScale() float32 {
	v := os.Getenv("BAOZI_SCALE")
	if v == "" {
		return 1
	}
	f, err := strconv.ParseFloat(v, 32)
	if err != nil || f <= 0 {
		return 1
	}
	return float32(f)
}

func TestPanelViewRenders(t *testing.T) {
	a := testApp(t)
	tester := ui.NewTester(a.view, panelWidth, panelHeight)
	tester.Frame()
	if !tester.HasText("开始输入以搜索文件和应用") {
		t.Fatal("空状态提示没有渲染出来")
	}
}

func TestSettingsViewRenders(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	tester := ui.NewTester(s.view, 880, 900)
	tester.Frame()
	for _, want := range []string{"Baozi 设置", "双击 Ctrl 唤出面板", "保存并应用"} {
		if !tester.HasText(want) {
			t.Fatalf("设置界面缺少 %q", want)
		}
	}
	// 文件对话框跟随那一块：总开关 + 六种目录来源，一个都不能少。
	for _, want := range []string{
		"文件对话框跟随", "启用跟随",
		"资源管理器", "Total Commander", "Directory Opus", "XYplorer", "xplorer2", "tessoa",
	} {
		if !tester.HasText(want) {
			t.Fatalf("设置界面缺少来源开关 %q", want)
		}
	}
	// 图标大小那一行：下拉在未展开时只画当前值，所以这里只确认这一行在，
	// 台阶本身由 TestIconScalePresets 直接查数据。
	if !tester.HasText("图标大小") {
		t.Fatal("设置界面缺少「图标大小」这一行")
	}
	dumpPreview(t, tester)
}

// 台阶本身：下拉里要有哪几档、当前倍数落在哪一档、手输的值如何归类。
func TestIconScalePresets(t *testing.T) {
	labels := iconScaleLabels()
	for _, want := range []string{"75%", "100%", "125%", "150%", "175%", "200%", iconCustomLabel} {
		found := false
		for _, l := range labels {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("台阶里缺少 %q，实际 %v", want, labels)
		}
	}

	// 1.25 落在 125% 这一档；1.3 不在任何台阶上，归自定义。
	if got := iconPresetLabel(1.25); got != "125%" {
		t.Fatalf("1.25 倍应当是 125%%，实际 %q", got)
	}
	if got := iconPresetLabel(1.3); got != iconCustomLabel {
		t.Fatalf("1.3 倍应当归自定义，实际 %q", got)
	}
	// 台阶标签要能换回倍数，否则选了下拉不会改值。
	for _, tc := range []struct {
		label string
		want  float64
	}{{"75%", 0.75}, {"175%", 1.75}, {"200%", 2}} {
		got, ok := iconPresetValue(tc.label)
		if !ok || got != tc.want {
			t.Fatalf("%s 应当对应 %v，实际 %v（%v）", tc.label, tc.want, got, ok)
		}
	}
	if _, ok := iconPresetValue(iconCustomLabel); ok {
		t.Fatal("自定义不该对应任何固定倍数")
	}
}

// 「自定义」填的是像素：27.5 的基准下填 33，折成 1.2 倍存下去，
// 再打开设置要能原样回到 33 像素。两头只要有一头错，用户填的数就会跳。
func TestIconPxCustom(t *testing.T) {
	a := testApp(t)
	a.scale = 1.25 // 125% 屏：默认大小 27.5 像素
	s := &settingsWindow{app: a}
	s.load()
	if got := s.iconBasePx(); got != 27.5 {
		t.Fatalf("125%% 缩放下的默认大小应是 27.5，实际 %v", got)
	}
	s.iconPx = 33
	s.iconScale = config.ClampIconScale(s.iconPx / s.iconBasePx())
	if got := s.iconPreview(); got != "33.0 像素" {
		t.Fatalf("填 33 像素时预览应显示 33.0 像素，实际 %q", got)
	}

	// 存盘后重新打开：像素数要回来，下拉也要认得这不是任何一个台阶。
	a.cfg.Search.IconScale = s.iconScale
	a.cfg.Search.IconSize = 0
	s2 := &settingsWindow{app: a}
	s2.load()
	if got := s2.iconPreview(); got != "33.0 像素" {
		t.Fatalf("重新打开设置后应还是 33.0 像素，实际 %q", got)
	}
	if s2.iconPreset != iconCustomLabel {
		t.Fatalf("33 像素不在任何台阶上，应当归自定义，实际 %q", s2.iconPreset)
	}
}

// 「自定义」那一档：下拉停在自定义时才露出像素输入框，旁边读数跟着它走。
func TestSettingsIconCustomShowsPx(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	s.iconPreset = iconCustomLabel
	s.iconPx = 30
	s.iconScale = config.ClampIconScale(s.iconPx / s.iconBasePx())
	tester := ui.NewTester(s.view, 880, 900)
	tester.Frame()
	if !tester.HasText("30.0 像素") {
		t.Fatal("自定义像素那一档没有把 30 像素摆出来")
	}
	dumpPreview(t, tester)
}

// 图标改成倍数之后，放大必须真的连锁到布局：卡片变宽、同一块面板一排能放的卡变少。
// 只改数字不动布局，就等于"设置里调了没用"。
func TestIconScaleWidensCards(t *testing.T) {
	a := testApp(t)
	a.cfg.Search.IconSize = 0
	a.cfg.Search.IconScale = 1

	baseW, baseCols := a.cellWidth(), a.gridCols()
	if baseW != cardWidthMin {
		t.Fatalf("100%% 倍时卡片应是最小宽度 %v，实际 %v", float32(cardWidthMin), baseW)
	}

	a.cfg.Search.IconScale = 2
	if a.cellWidth() <= baseW {
		t.Fatalf("200%% 倍时卡片应当变宽，实际还是 %v", a.cellWidth())
	}
	if a.gridCols() >= baseCols {
		t.Fatalf("卡片变宽之后一排应当放不下那么多了，实际 %d → %d", baseCols, a.gridCols())
	}
	// 倍数要真的乘进图标尺寸，而不是被夹取吃掉。
	if a.iconSize() != config.BaseIconSize*2 {
		t.Fatalf("200%% 倍时图标应是 %v，实际 %v", config.BaseIconSize*2, a.iconSize())
	}
}

// dumpPreview 在 BAOZI_PREVIEW=<绝对路径> 时把这一帧存成 PNG，
// 靠肉眼核对布局（挤压、换行、对齐）。平时不写盘。
func dumpPreview(t *testing.T, tr *ui.Tester) {
	t.Helper()
	p := os.Getenv("BAOZI_PREVIEW")
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建预览目录: %v", err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建预览文件: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, tr.Image()); err != nil {
		t.Fatalf("写预览 PNG: %v", err)
	}
	t.Logf("预览已写入 %s", p)
}

func TestSettingsParseCommands(t *testing.T) {
	cases := []struct {
		in   string
		want int
		kw   string
	}{
		{"term | Windows Terminal | wt.exe", 1, "term"},
		{"code | VS Code | code | --new-window", 1, "code"},
		{"term | wt.exe\ncode | code", 2, "term"},
		{"", 0, ""},
	}
	for _, c := range cases {
		got := parseCommands(c.in)
		if len(got) != c.want {
			t.Fatalf("%q 解析出 %d 条，期望 %d", c.in, len(got), c.want)
		}
		if c.want > 0 && got[0].Keyword != c.kw {
			t.Fatalf("%q 首个关键字 = %q", c.in, got[0].Keyword)
		}
		if c.want > 1 && got[1].Exec != "code" {
			t.Fatalf("第二行 exec = %q", got[1].Exec)
		}
	}
}

func TestShareViewRenders(t *testing.T) {
	a := testApp(t)
	sess, err := share.Start("", time.Hour)
	if err != nil {
		t.Fatalf("share.Start: %v", err)
	}
	defer sess.Stop()
	src := filepath.Join(a.dataDir, "demo.txt")
	if err := os.WriteFile(src, []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess.Add(src)

	s := &shareWindow{app: a, session: sess}
	tester := ui.NewTester(s.view, 760, 680)
	tester.Frame()
	// 访问码要单独可见（4 位、手敲用），网卡下拉也要在。
	// 「添加文件 / 添加文件夹」固定在滚动区外面，任何窗口高度下都得看得见
	// ——这两个按钮之前在滚动区底部，内容一多就被顶出可视范围，用户以为按钮没了。
	for _, want := range []string{"文件互传", "demo.txt", "复制链接", "访问码", sess.Token(), "绑定网卡", "添加文件", "添加文件夹", "链接有效期", "到期"} {
		if !tester.HasText(want) {
			t.Fatalf("互传界面缺少 %q", want)
		}
	}
	// 窗口缩到最小高度也要看得到
	small := ui.NewTester(s.view, 640, 520)
	small.Frame()
	for _, want := range []string{"添加文件", "添加文件夹", "停止共享"} {
		if !small.HasText(want) {
			t.Fatalf("最小尺寸下缺少 %q", want)
		}
	}

	// 还没生成链接时（session 为空）：入口按钮是「生成链接」，且已挑好的文件要能看见。
	stopped := &shareWindow{app: a, pending: []share.Item{{Path: src, Name: "demo.txt", Size: 4}}}
	st := ui.NewTester(stopped.view, 640, 520)
	st.Frame()
	for _, want := range []string{"生成链接", "添加文件", "添加文件夹", "待分享", "demo.txt", "链接有效期"} {
		if !st.HasText(want) {
			t.Fatalf("未共享状态缺少 %q", want)
		}
	}
}

func TestHistoryToResults(t *testing.T) {
	a := testApp(t)
	a.hist.TouchEntry(history.Entry{
		Path: `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`,
		Name: "计算器", Kind: history.KindApp, Ext: ".exe", Hint: "应用",
	})
	a.hist.TouchEntry(history.Entry{
		Path: `D:\docs\合同.docx`,
		Name: "合同.docx", Kind: history.KindFile, Ext: ".docx", Hint: `D:\docs`,
	})
	got := a.historyResults(5)
	if len(got) != 2 {
		t.Fatalf("应有 2 条历史，实际 %d", len(got))
	}
	for _, r := range got {
		switch r.Path {
		case `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`:
			if r.Name != "计算器" {
				t.Fatalf("应用历史名字被还原成 %q", r.Name)
			}
			if r.Kind != 2 { // search.KindApp
				t.Fatalf("应用历史类型 = %d，期望 KindApp", r.Kind)
			}
		case `D:\docs\合同.docx`:
			if r.Name != "合同.docx" || r.Kind != 0 {
				t.Fatalf("文件历史还原错误: %+v", r)
			}
		}
	}
}

// seedSearchApp 造一个"开始菜单应用 + 同名文件"的场景，用来验证
// (1) 拼音首字母能搜到中文应用名，(2) 类型权重让应用压过更准确的文件。
func seedSearchApp(t *testing.T, a *application) {
	t.Helper()
	appPath := `shell:AppsFolder\Doubao.Office_1.0.0.0_x64!App`
	apps := []appEntry{{
		InstalledApp: mygo.InstalledApp{Name: "豆包工作", ID: "Doubao.Office_1.0.0.0_x64!App", Path: appPath},
		initials:     "dbgz",
		full:         "doubaogongzuo",
	}}
	a.apps.Store(&apps)

	tab := &index.Table{
		Nodes: []index.Node{
			{Parent: -1, Kind: index.KindDir, NameOff: 0, NameLen: 3},
			{Parent: 0, Kind: index.KindFile, NameOff: 3, NameLen: 10},
			{Parent: 0, Kind: index.KindDir, NameOff: 13, NameLen: 12},
		},
		Names: []byte(`C:\doubao.exe豆包工作` + "dbgz"), // 拼音串占 arena 尾部（off 25，长 4）
		Py:    map[int32]uint64{2: index.PackPy(25, 4)},
	}
	a.cat.Store(tab)
}

func TestPinyinFindsChineseApp(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)

	got := a.searchAll("dbgz")
	if len(got) == 0 {
		t.Fatal("输入 dbgz 应当命中「豆包工作」")
	}
	first := got[0]
	if first.Kind != search.KindApp {
		t.Fatalf("首条应是应用，实际 Kind=%d（%s）", first.Kind, first.Name)
	}
	if first.Name != "豆包工作" {
		t.Fatalf("首条名字 = %q，期望「豆包工作」", first.Name)
	}
	if first.Accuracy < 0.95 {
		t.Fatalf("拼音全等的准确度只有 %.3f", first.Accuracy)
	}

	// 同名的目录也在结果里，但类型分低，必须排到应用后面
	for i, r := range got {
		if r.Name == "豆包工作" && r.Kind == search.KindDir && i < 1 {
			t.Fatal("目录不该压过应用")
		}
	}
}

func TestAppBeatsExeInSearch(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)

	// doubao 只命中了应用的全拼（0.88）和文件的名字前缀（0.95），
	// 准确度是文件更高；但用户要的第一权重是类型，所以应用仍该在前。
	got := a.searchAll("doubao")
	if len(got) < 2 {
		t.Fatalf("应有应用和文件两条结果，实际 %d", len(got))
	}
	if got[0].Kind != search.KindApp {
		t.Fatalf("首条应是应用，实际 %s (Kind=%d)", got[0].Name, got[0].Kind)
	}
}

// 首页网格：固定项排在前面，pinnedCount 与「已固定」段一致。
func TestHomeGridSplitsPinned(t *testing.T) {
	a := testApp(t)
	for _, p := range []string{"a", "b", "c"} {
		a.hist.TouchEntry(history.Entry{Path: p, Name: p, Kind: history.KindApp})
	}
	if _, ok := a.hist.TogglePin("b"); !ok {
		t.Fatal("固定失败")
	}
	a.refilter()
	if a.pinnedCount != 1 {
		t.Fatalf("pinnedCount=%d，应为 1", a.pinnedCount)
	}
	if len(a.results) != 3 {
		t.Fatalf("总数 %d，应为 3", len(a.results))
	}
	if a.results[len(a.results)-1].Path != "b" {
		t.Fatalf("末条应是固定的 b（固定沉底），实际 %s", a.results[len(a.results)-1].Path)
	}
}

// Ctrl+K 在输入框有焦点时也要能固定 / 取消固定（Pane 的交互）。
func TestCtrlKPinsFromKeyboard(t *testing.T) {
	a := testApp(t)
	const exe = `C:\Windows\notepad.exe`
	a.hist.TouchEntry(history.Entry{Path: exe, Name: "记事本", Kind: history.KindApp, Ext: ".exe", Hint: "应用"})
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	tr.Key(ui.Ctrl, ui.KeyK)
	tr.Frame()
	if !a.hist.IsPinned(exe) {
		t.Fatal("Ctrl+K 没有固定选中的条目")
	}
	tr.Key(ui.Ctrl, ui.KeyK)
	tr.Frame()
	if a.hist.IsPinned(exe) {
		t.Fatal("再按一次 Ctrl+K 应该取消固定")
	}
}

// 首页方向键按行移动（一行 panelCols 个）。
// 面板宽度可配置之后，一排几张卡要跟着变——不只是"能不能塞下"，方向键按行移动、
// 拖动换位也都吃这个数，所以三者必须同源。这里钉住换算本身。
func TestGridColsFollowsPanelWidth(t *testing.T) {
	a := &application{cfg: &config.Config{}}
	if got := a.gridCols(); got != panelCols {
		t.Fatalf("默认 %d 宽时应为 %d 列，实际 %d", panelWidth, panelCols, got)
	}
	a.cfg.Panel.Width = 1200
	if got := a.gridCols(); got != 12 {
		t.Fatalf("1200 宽时一排 12 列，实际 %d", got)
	}
	a.cfg.Panel.Width = 480
	if got := a.gridCols(); got != 4 {
		t.Fatalf("480 宽时一排 4 列，实际 %d", got)
	}
	// 再窄也得留一列，否则首页网格会一行都不画（那是彻底的空白）。
	a.cfg.Panel.Width = config.MinPanelWidth
	if got := a.gridCols(); got < 1 {
		t.Fatalf("极窄面板也至少要 1 列，实际 %d", got)
	}
}

func TestHomeGridArrowMovesByRow(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 10; i++ {
		a.hist.TouchEntry(history.Entry{Path: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("item%d", i), Kind: history.KindApp})
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	tr.Key(0, ui.KeyDown)
	tr.Frame()
	if a.chosen != panelCols {
		t.Fatalf("Down 之后 chosen=%d，应为 %d", a.chosen, panelCols)
	}
	tr.Key(0, ui.KeyUp)
	tr.Frame()
	if a.chosen != 0 {
		t.Fatalf("Up 之后 chosen=%d，应为 0", a.chosen)
	}
	// 左右是 ±1（首行从左往右走）。
	tr.Key(0, ui.KeyRight)
	tr.Frame()
	if a.chosen != 1 {
		t.Fatalf("Right 之后 chosen=%d，应为 1", a.chosen)
	}
	tr.Key(0, ui.KeyLeft)
	tr.Frame()
	if a.chosen != 0 {
		t.Fatalf("Left 之后 chosen=%d，应为 0", a.chosen)
	}
}

// 焦点在搜索框里时方向键也要能动：TextInput 会把左右键吃掉（光标移动），
// 所以首页的方向键读的是按住状态，而不是 Shortcut。
func TestHomeArrowsWorkWithInputFocus(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 10; i++ {
		a.hist.TouchEntry(history.Entry{Path: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("item%d", i), Kind: history.KindApp})
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	tr.ClickAt(300, 30)
	tr.Frame()
	if !tr.Focused("搜索") {
		t.Fatal("点击没能把焦点落到搜索框，测试前提没成立")
	}
	tr.Key(0, ui.KeyRight)
	tr.Frame()
	if a.chosen != 1 {
		t.Fatalf("焦点在搜索框时 Right 之后 chosen=%d，应为 1", a.chosen)
	}
	tr.Key(0, ui.KeyDown)
	tr.Frame()
	if a.chosen != 1+panelCols {
		t.Fatalf("焦点在搜索框时 Down 之后 chosen=%d，应为 %d", a.chosen, 1+panelCols)
	}
}

// 首页卡片的名字要尽量完整显示：37 个字符的英文文件名在三行里能全放进去，
// 只有真正放不下的才加省略号。
func TestWrapName(t *testing.T) {
	if got := wrapName("微信", 76, 3); got != "微信" {
		t.Fatalf("短名字不该被折行: %q", got)
	}
	long := "SnowShot_Video_2026-10-29_13-31-04.mp4"
	got := wrapName(long, 76, 3)
	if strings.Contains(got, "…") {
		t.Fatalf("37 个字符的名字应当完整放进三行: %q", got)
	}
	if n := strings.Count(got, "\n") + 1; n != 3 {
		t.Fatalf("应当折成 3 行，实际 %d 行: %q", n, got)
	}
	if !strings.HasSuffix(strings.ReplaceAll(got, "\n", ""), ".mp4") {
		t.Fatalf("折行后不该丢字: %q", got)
	}
	if huge := wrapName(strings.Repeat("A", 60), 76, 3); !strings.Contains(huge, "…") {
		t.Fatalf("三行放不下的长名要加省略号: %q", huge)
	}
	// 中文按整字折，不能把一个汉字劈成两半。
	cn := "一个特别长的中文项目名字用来测试换行效果"
	if got := wrapName(cn, 76, 3); strings.Contains(got, "…") {
		t.Fatalf("20 个汉字应当完整放进三行: %q", got)
	}
}

// 搜索结果的鼠标点击要能打开：之前行上没有任何点击处理，点了像没反应。
func TestResultRowClickOpens(t *testing.T) {
	a := testApp(t)
	a.query = "demo"
	a.results = []search.Result{{
		Path: `D:\x\demo.txt`, Name: "demo.txt", Kind: search.KindFile, Hint: `D:\x`,
	}}
	a.chosen = 0
	stubAllIcons(a) // offline：open() 不再真去 Shell.OpenPath（会永久阻塞）
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	rect, ok := tr.Find("demo.txt")
	if !ok {
		t.Fatal("渲染后找不到 demo.txt 这一行")
	}
	tr.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tr.Frame()
	if a.hist.Count(`D:\x\demo.txt`) == 0 {
		t.Fatal("点了结果行没有打开（历史里没有记一笔）")
	}
}

// 首页卡片的右键菜单要能删掉「最近使用」里的条目。
func TestHomeCardContextMenuDelete(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 2; i++ {
		a.hist.TouchEntry(history.Entry{Path: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("item%d", i), Kind: history.KindApp})
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	if err := tr.RightClick("item0"); err != nil {
		t.Fatalf("右键 item0: %v", err)
	}
	menu := tr.Menu()
	if len(menu) == 0 {
		t.Fatal("卡片上没有弹出右键菜单")
	}
	if err := tr.ChooseMenuItem("从历史删除"); err != nil {
		t.Fatalf("选「从历史删除」: %v", err)
	}
	tr.Frame() // applyPending 在下一帧开头执行
	if a.hist.Count("p0") != 0 {
		t.Fatal("删了之后历史里还有这条")
	}
	if tr.HasText("item0") {
		t.Fatal("首页还留着被删掉的卡片")
	}
	if !tr.HasText("item1") {
		t.Fatal("删一条却把别的也删了")
	}
}

// 右键菜单里要有「打开所在文件夹」：和 Ctrl+Enter 是同一个动作，
// 只是这里走被右键点中的那一条，而不是当前选中项。
func TestContextMenuHasOpenFolder(t *testing.T) {
	a := testApp(t)
	a.hist.TouchEntry(history.Entry{Path: `C:\Windows\notepad.exe`, Name: "记事本", Kind: history.KindApp})
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	if err := tr.RightClick("记事本"); err != nil {
		t.Fatalf("右键记事本：%v", err)
	}
	menu := tr.Menu()
	found := false
	for _, label := range menu {
		if label == "打开所在文件夹" {
			found = true
		}
	}
	if !found {
		t.Fatalf("右键菜单里没有「打开所在文件夹」，实际菜单：%v", menu)
	}
	// 只确认在菜单里，不真的选它：那会去壳里开资源管理器。
}

// 命令类结果没有真实路径，那一项要灰着不能点。
func TestCommandResultOpenFolderDisabled(t *testing.T) {
	a := testApp(t)
	a.query = "lock"
	a.results = []search.Result{{Name: "锁屏", Kind: search.KindCommand}}
	a.chosen = 0
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	rect, ok := tr.Find("锁屏")
	if !ok {
		t.Fatal("渲染后找不到「锁屏」这一行")
	}
	tr.RightClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	if err := tr.ChooseMenuItem("打开所在文件夹"); err == nil {
		t.Fatal("命令类结果也能选「打开所在文件夹」，应被禁用")
	}
}

// 固定项可以从菜单里取消固定，取消后回到「最近使用」。
func TestPinnedCardUnpin(t *testing.T) {
	a := testApp(t)
	a.hist.TouchEntry(history.Entry{Path: "p0", Name: "item0", Kind: history.KindApp})
	a.hist.TogglePin("p0")
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	if err := tr.RightClick("item0"); err != nil {
		t.Fatalf("右键 item0: %v", err)
	}
	if err := tr.ChooseMenuItem("取消固定"); err != nil {
		t.Fatalf("选「取消固定」: %v", err)
	}
	tr.Frame()
	if a.hist.IsPinned("p0") {
		t.Fatal("取消固定没生效")
	}
	if a.pinnedCount != 0 {
		t.Fatalf("取消固定后 pinnedCount = %d，应为 0", a.pinnedCount)
	}
}

// Ctrl+Shift+C 复制的目录口径：目录给自身，文件给所在目录，虚拟项没有目录。
func TestDirOf(t *testing.T) {
	cases := []struct {
		path, name string
		kind       search.Kind
		want       string
	}{
		{`D:\work\baozi\README.md`, "README.md", search.KindFile, `D:\work\baozi`},
		{`D:\work\baozi`, "baozi", search.KindDir, `D:\work\baozi`},
		{"shell:AppsFolder", "某个应用", search.KindApp, ""},
		{"", "命令", search.KindCommand, ""},
	}
	for _, c := range cases {
		got := dirOf(search.Result{Path: c.path, Name: c.name, Kind: c.kind})
		if got != c.want {
			t.Fatalf("%q (%v) 的目录应为 %q，实际 %q", c.path, c.kind, c.want, got)
		}
	}
}

// 底栏要写出 Ctrl+Shift+C 这条键帽，否则用户不知道有这个动作。
// 底栏的两个入口（设置 / 互传）必须是看得出来的按钮，不能又缩回键帽那种浅灰小方块——
// 它们跟「Enter 打开」不是一个层级：后者是提示，前者是能点的东西。
func TestFooterHasEntryButtons(t *testing.T) {
	a := testApp(t)
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	for _, want := range []string{"设置", "互传"} {
		if !tr.HasText(want) {
			t.Fatalf("底栏缺少入口 %q", want)
		}
	}
	// 这个测试也顺带出图：底栏的图标是位图（不是文字），只断言文字看不出它画没画出来。
	dumpPreview(t, tr)
}

func TestFooterShowsCopyPathKey(t *testing.T) {
	a := testApp(t)
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	if !tr.HasText("Ctrl+Shift+C") {
		t.Fatal("底栏没有 Ctrl+Shift+C 复制路径的提示")
	}
}

// .exe/.lnk 虽然记成文件，但要取系统彩色图标（线框认不出是哪个程序）。
func TestIsProgram(t *testing.T) {
	cases := []struct {
		path, ext string
		kind      search.Kind
		want      bool
	}{
		{`D:\1tools\HiBitUninstaller.exe`, ".exe", search.KindFile, true},
		{`C:\Users\Administrator\Desktop\微信.lnk`, ".lnk", search.KindFile, true},
		{`D:\setup.msi`, ".msi", search.KindFile, true},
		{`D:\run.bat`, ".bat", search.KindFile, false}, // 齿轮图标，保持线框
		{`D:\work\baozi\README.md`, ".md", search.KindFile, false},
		{"shell:AppsFolder\\某个应用", "", search.KindApp, false}, // 应用本来就走彩色
		{`D:\work\baozi`, "", search.KindDir, false},
	}
	for _, c := range cases {
		got := isProgram(search.Result{Path: c.path, Ext: c.ext, Kind: c.kind})
		if got != c.want {
			t.Fatalf("%q (%s, kind=%v) 应返回 %v，实际 %v", c.path, c.ext, c.kind, c.want, got)
		}
	}
}

// 图标调大以后，结果行必须真的跟着变高——只改一个数字而布局不动，就等于设置没生效。
// 这里用"第二行名字的纵坐标"当标尺：两行结果，第一行吃掉更多高度，第二行就会更低。
func TestIconSizeSettingGrowsRows(t *testing.T) {
	a := testApp(t)
	a.offline = true
	a.query = "zz"
	a.results = []search.Result{
		{Item: search.Item{Name: "zz-one.txt", Path: `C:\zz-one.txt`, Ext: ".txt", Kind: search.KindFile, Hint: `C:\`}},
		{Item: search.Item{Name: "zz-two.txt", Path: `C:\zz-two.txt`, Ext: ".txt", Kind: search.KindFile, Hint: `C:\`}},
	}
	a.choose(0)

	// 第二行名字的纵坐标
	secondRowY := func() float32 {
		stubAllIcons(a)
		tr := ui.NewTester(a.view, panelWidth, panelHeight)
		tr.Frame()
		rect, ok := tr.Find("zz-two.txt")
		if !ok {
			t.Fatal("第二行结果没渲染出来")
		}
		return rect.Y
	}

	// a.scale 在测试里是 0，IconSizeAt 会退回 1.0，也就是 BaseIconSize。
	small := secondRowY()
	if got := a.iconSize(); got != config.BaseIconSize {
		t.Fatalf("缩放未知时图标应退回 %d，实际 %v", config.BaseIconSize, got)
	}

	// 注意这里要开到 60 而不是 44：行高是"图标"和"名字+提示两行字"取高的那个，
	// 名字那列本身就有 38 左右，图标不超过它就不会把行撑开（这是好事——默认
	// 22→27.5 只是图标变大、显示行数不变）。60 才能证明图标真的进了布局。
	a.cfg.Search.IconSize = 60
	big := secondRowY()
	if got := a.iconSize(); got != 60 {
		t.Fatalf("设置里填了 60，图标应为 60，实际 %v", got)
	}
	if gap := big - small; gap < 12 {
		t.Fatalf("图标从 %d 调到 60 之后行高只涨了 %.1f，说明图标没真的进布局",
			config.BaseIconSize, gap)
	}
}

// 首页网格的实拍：固定项在前、后面跟着网格。BAOZI_PREVIEW=<绝对路径> 才写盘；
// 再加 BAOZI_ICONSCALE=1.75 就能看到"图标放大后卡片跟着变宽"的样子。
func TestGridPreviewShot(t *testing.T) {
	a := testApp(t)
	if v := os.Getenv("BAOZI_ICONSCALE"); v != "" {
		k, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatalf("BAOZI_ICONSCALE 不是数字: %v", v)
		}
		a.cfg.Search.IconSize = 0
		a.cfg.Search.IconScale = k
	}
	for _, e := range []history.Entry{
		{Path: `D:\Games\遗忘之海\game.exe`, Name: "遗忘之海", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\notepad.exe`, Name: "记事本", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\System32\cmd.exe`, Name: "命令提示符", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\System32\mstsc.exe`, Name: "远程桌面连接", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\System32\calc.exe`, Name: "计算器", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\System32\taskmgr.exe`, Name: "任务管理器", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\System32\mspaint.exe`, Name: "画图", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `C:\Windows\regedit.exe`, Name: "注册表编辑器", Kind: history.KindApp, Ext: ".exe", Hint: "应用"},
		{Path: `D:\work\baozi`, Name: "baozi", Kind: history.KindDir, Hint: `D:\work`},
	} {
		a.hist.TouchEntry(e)
	}
	if _, ok := a.hist.TogglePin(`D:\Games\遗忘之海\game.exe`); !ok {
		t.Fatal("固定失败")
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	dumpPreview(t, tr)
	t.Logf("缩放 %v，倍数 %.2f，图标 %.1f，卡片宽 %.1f，一排 %d 张",
		a.scale, a.cfg.Search.IconScale, a.cardIcon(), a.cellWidth(), a.gridCols())
}

// 图标大小的实拍。BAOZI_SCALE=1.25 BAOZI_PREVIEW=<绝对路径> 跑它，看到的就是
// "125% 缩放屏上的默认图标"；再加 BAOZI_ICONSCALE=1.75 就能看到"默认基础上再放大 1.75 倍"。
func TestIconPreviewShot(t *testing.T) {
	a := testApp(t)
	// 倍数走环境变量，便于同一套数据出几张小大不同的图做对比。
	if v := os.Getenv("BAOZI_ICONSCALE"); v != "" {
		k, err := strconv.ParseFloat(v, 64)
		if err != nil {
			t.Fatalf("BAOZI_ICONSCALE 不是数字: %v", v)
		}
		a.cfg.Search.IconSize = 0
		a.cfg.Search.IconScale = k
	}
	a.offline = true
	a.query = "zz"
	a.results = []search.Result{
		{Item: search.Item{Name: "zz-季度报告.pdf", Path: `D:\docs\zz-季度报告.pdf`, Ext: ".pdf", Kind: search.KindFile, Hint: `D:\docs`}},
		{Item: search.Item{Name: "zz-work", Path: `D:\work\zz-work`, Kind: search.KindDir, Hint: `D:\work`}},
		{Item: search.Item{Name: "zz-tools.exe", Path: `D:\1tools\zz-tools.exe`, Ext: ".exe", Kind: search.KindFile, Hint: `D:\1tools`}},
		{Item: search.Item{Name: "zz-notes.txt", Path: `D:\docs\zz-notes.txt`, Ext: ".txt", Kind: search.KindFile, Hint: `D:\docs`}},
	}
	a.choose(0)
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	dumpPreview(t, tr)
	if a.iconSize() <= 0 {
		t.Fatal("图标尺寸算成了 0，那样什么都画不出来")
	}
	t.Logf("缩放 %v，图标 %.1f", a.scale, a.iconSize())
}

// iconPath 只剥"shell:AppsFolder\ + 真路径"这种畸形写法；正常 AUMID 必须原样
// 交给 Shell（按扩展名当文件取图标会拿到一个不相干的通用图标）。
func TestIconPathStripsFilePrefix(t *testing.T) {
	aumid := `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`
	if got := iconPath(aumid); got != aumid {
		t.Fatalf("正常 AUMID 不该被改：%q", got)
	}
	bad := `shell:AppsFolder\D:\Games\ywzh_launcher\Win64Release\PCLauncher\PCLauncher.exe`
	want := `D:\Games\ywzh_launcher\Win64Release\PCLauncher\PCLauncher.exe`
	if got := iconPath(bad); got != want {
		t.Fatalf("畸形路径应剥成 %q，实际 %q", want, got)
	}
	plain := `C:\Windows\System32\notepad.exe`
	if got := iconPath(plain); got != plain {
		t.Fatalf("普通路径不该被改：%q", got)
	}
	// 大小写不同也要认（前缀是 Word 拼出来的，别指望大小写稳定）。
	upper := `SHELL:APPSFOLDER\D:\a\b.exe`
	if got := iconPath(upper); got != `D:\a\b.exe` {
		t.Fatalf("前缀大小写不同也应剥掉，实际 %q", got)
	}
}

// 悬停选中的关键是"指针没动就不接管"：否则鼠标停在 A 上、用户按 ↓ 选 B，
// 下一帧 hover 又把人拽回 A，键盘等于失灵。这里把三种情形钉住。
func TestHoverTargetKeepsKeyboardChoice(t *testing.T) {
	p := ui.Point{X: 10, Y: 10}
	// 首帧（还没有上一帧位置）：接管指针下面那张。
	if got, _, _ := hoverTarget(ui.Point{}, false, p, 0, 3); got != 3 {
		t.Fatalf("首帧应接管为 3，实际 %d", got)
	}
	// 位置没变：键盘刚把选中从 3 挪到 1，hover 不许改回去。
	if got, _, _ := hoverTarget(p, true, p, 1, 3); got != 1 {
		t.Fatalf("指针没动应保持键盘选的 1，实际 %d", got)
	}
	// 位置变了：接管。
	moved := ui.Point{X: 120, Y: 10}
	if got, _, _ := hoverTarget(p, true, moved, 1, 3); got != 3 {
		t.Fatalf("指针移动应接管为 3，实际 %d", got)
	}
}

// 跨网那一页要有自己的样子：两个页签、信令地址、发送/接收两栏。
// 这里渲染它，并在设置 BAOZI_PREVIEW 时出图，肉眼核对有没有挤在一起。
// 发送方等对方进来时的样子：短码要大，下面还要给一条"对方不装 Baozi 也能收"的网页地址。
func TestShareP2PWaitingShot(t *testing.T) {
	a := testApp(t)
	s := &shareWindow{app: a, tab: 1}
	a.cfg.Share.Signal = ""
	s.signalInput = s.signalBase()
	s.net = &netShare{
		win:   s,
		role:  "sender",
		stage: p2pWaiting,
		code:  "bzt234",
		note:  "已连上信令，等对方进入房间",
	}

	tester := ui.NewTester(s.view, 760, 620)
	tester.Frame()
	for _, want := range []string{"短码", "bzt234", "链接", "复制链接", "取消"} {
		if !tester.HasText(want) {
			t.Fatalf("等待态缺少 %q", want)
		}
	}
	dumpPreview(t, tester)
}

func TestShareP2PViewRenders(t *testing.T) {
	a := testApp(t)
	s := &shareWindow{app: a, tab: 1}
	a.cfg.Share.Signal = ""
	s.signalInput = s.signalBase()
	s.net = &netShare{win: s}
	s.net.dir = `D:\Downloads`

	tester := ui.NewTester(s.view, 760, 680)
	tester.Frame()
	for _, want := range []string{
		"局域网链接", "跨网直传", "信令服务", "生成短码", "添加文件", "添加文件夹", "开始接收",
	} {
		if !tester.HasText(want) {
			t.Fatalf("跨网页缺少 %q", want)
		}
	}
	dumpPreview(t, tester)
}

// 只收到一段文本的完成态：必须铺开、能复制，而不是只报一句"什么也没收到"。
func TestShareP2PTextResultShot(t *testing.T) {
	a := testApp(t)
	s := &shareWindow{app: a, tab: 1}
	a.cfg.Share.Signal = ""
	s.signalInput = s.signalBase()
	s.net = &netShare{
		win:     s,
		role:    "receiver",
		stage:   p2pDone,
		note:    "收到一段文本，可以直接复制走了",
		via:     "直连",
		gotText: "第一行：贴一条命令\n第二行：curl -fsSL https://example.com/x.sh | sh",
	}

	tester := ui.NewTester(s.view, 760, 720)
	tester.Frame()
	// TextArea 里那几行正文 HasText 抓不到（只读文本域不算文本节点），这里只钉结构与按钮。
	for _, want := range []string{"收到的文本", "复制文本", "个字符", "再来一次"} {
		if !tester.HasText(want) {
			t.Fatalf("文本完成态缺少 %q", want)
		}
	}
	dumpPreview(t, tester)
}

// 「测试」的结果必须出现在信令行下面——跨网页签没有底栏，早先只写 status
// 等于点了没反应。顺带钉住那条细状态线，别再把反馈收没了。
func TestShareSignalTestResultShot(t *testing.T) {
	a := testApp(t)
	s := &shareWindow{
		app:        a,
		tab:        1,
		signalTest: signalBad,
		signalMsg:  "连不上 https://signal.1jia.life：连接超时",
		status:     "信令服务连不上",
	}
	a.cfg.Share.Signal = ""
	s.signalInput = s.signalBase()
	s.net = &netShare{win: s}

	tester := ui.NewTester(s.view, 760, 680)
	tester.Frame()
	for _, want := range []string{"测试", "连不上", "连接超时", "信令服务连不上"} {
		if !tester.HasText(want) {
			t.Fatalf("测试结果没显示出来，缺少 %q", want)
		}
	}
	dumpPreview(t, tester)

	// 成功态也得能看见，并且带耗时。
	s.signalTest, s.signalMsg = signalOK, "https://signal.1jia.life 能用，412ms 就有回应"
	ok := ui.NewTester(s.view, 760, 680)
	ok.Frame()
	for _, want := range []string{"能用", "412ms"} {
		if !ok.HasText(want) {
			t.Fatalf("成功态缺少 %q", want)
		}
	}
}

// 局域网页也能顺带发一段文本：停了共享的状态下必须有那个输入框，
// 否则用户只会想到"把话存成 txt 再发"。
func TestShareLocalTextInputShot(t *testing.T) {
	a := testApp(t)
	src := filepath.Join(a.dataDir, "demo.txt")
	if err := os.WriteFile(src, []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &shareWindow{
		app:       a,
		pending:   []share.Item{{Path: src, Name: "demo.txt", Size: 4}},
		textDraft: "这里是顺手要递过去的一段话",
	}

	tester := ui.NewTester(s.view, 760, 720)
	tester.Frame()
	for _, want := range []string{"生成链接", "待分享", "demo.txt", "顺带发一段文本"} {
		if !tester.HasText(want) {
			t.Fatalf("局域网页缺少 %q", want)
		}
	}
	dumpPreview(t, tester)
}
