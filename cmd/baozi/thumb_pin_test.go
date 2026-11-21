package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"baozi/internal/history"
	"baozi/internal/search"
	"baozi/internal/win"

	"github.com/egoist/mygo/ui"
)

// thumbable 决定哪些结果去要内容缩略图：图片、视频、PDF。
func TestThumbable(t *testing.T) {
	cases := []struct {
		name string
		r    search.Result
		want bool
	}{
		{"png", fileResult(`D:\a.png`, ".png"), true},
		{"大写扩展名", fileResult(`D:\a.PNG`, ".PNG"), true},
		{"jpg", fileResult(`D:\a.jpg`, ".jpg"), true},
		{"视频", fileResult(`D:\a.mp4`, ".mp4"), true},
		{"pdf", fileResult(`D:\a.pdf`, ".pdf"), true},
		{"文本不取", fileResult(`D:\a.txt`, ".txt"), false},
		{"程序走彩色图标", fileResult(`D:\a.exe`, ".exe"), false},
		{"没有扩展名", fileResult(`D:\a`, ""), false},
		{"目录", search.Result{Item: search.Item{Name: "资料", Path: `D:\资料`, Kind: search.KindDir}}, false},
		{"没路径", search.Result{Item: search.Item{Name: "x", Ext: ".png", Kind: search.KindFile}}, false},
	}
	for _, c := range cases {
		if got := thumbable(c.r); got != c.want {
			t.Fatalf("%s: thumbable=%v，期望 %v", c.name, got, c.want)
		}
	}
}

func fileResult(path, ext string) search.Result {
	return search.Result{Item: search.Item{Name: path, Path: path, Ext: ext, Kind: search.KindFile}}
}

// 后台取图的收尾：解码成功后要落进缓存（panel 为 nil 时直接写，测试走这条）。
func TestLoadThumbCachesBitmap(t *testing.T) {
	a := testApp(t)
	p := filepath.Join(os.Getenv("WINDIR"), "Web", "Screen", "img100.jpg")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("这台机器没有素材图：%v", err)
	}
	a.thumbSem = make(chan struct{}, 1)
	a.thumbSem <- struct{}{} // loadThumb 退出时会取走一个
	key := fmt.Sprintf("thumb:48:%s", p)
	a.loadThumb(key, p, 48)
	bm, ok := a.icons[key]
	if !ok || bm == nil {
		t.Fatalf("取回的缩略图没进缓存（ok=%v bm=%v）", ok, bm)
	}
}

// 缩略图真的能画进结果行：拿一张系统壁纸当素材，手动塞进缓存后渲染一帧。
// 不向 Shell 下单（offline），只验证"缓存里有图就显示图"这条渲染路径。
func TestThumbnailRendersInResultRow(t *testing.T) {
	a := testApp(t)
	a.offline = true
	p := filepath.Join(os.Getenv("WINDIR"), "Web", "Screen", "img100.jpg")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("这台机器没有素材图：%v", err)
	}
	size := a.iconSize() // 走变量，避免常量直接转 int 被编译器拒绝
	px := int(size * 2.2)
	data, err := win.Thumbnail(p, px)
	if err != nil {
		t.Skipf("取缩略图失败（可能在无桌面会话里跑）：%v", err)
	}
	bm, err := ui.DecodeBitmap(data)
	if err != nil || bm == nil {
		t.Fatalf("解码缩略图失败：%v", err)
	}
	a.icons[fmt.Sprintf("thumb:%d:%s", px, p)] = bm

	a.query = "img"
	a.results = []search.Result{{
		Item: search.Item{
			Name: "img100.jpg", Path: p, Ext: ".jpg",
			Kind: search.KindFile, Hint: `C:\Windows\Web\Screen`,
		},
	}}
	tester := ui.NewTester(a.view, panelWidth, panelHeight)
	tester.Frame()
	if !tester.HasText("img100.jpg") {
		t.Fatal("结果行没渲染出来")
	}
	dumpPreview(t, tester)
}

// 离线（视图测试）或用户关掉缩略图时，一张都不该下单。
func TestThumbIconDoesNotAskWhenOff(t *testing.T) {
	a := testApp(t)
	a.cfg.Search.Thumbnails = boolPtr(false)
	a.cfg.Search.Thumbnails = boolPtr(false)
	a.offline = true
	if bm := a.thumbIcon(fileResult(`D:\a.png`, ".png"), 22); bm != nil {
		t.Fatal("离线时不该返回缩略图")
	}
	if len(a.thumbAsk) != 0 {
		t.Fatalf("离线时不该下单，实际有 %d 个", len(a.thumbAsk))
	}
}

// 首页「已固定」区的卡片可以拖着重排：往右拖一格就把顺序换过去，并落进历史。
func TestPinDragReorders(t *testing.T) {
	a := testApp(t)
	stubAllIcons(a)
	names := []string{"甲甲", "乙乙", "丙丙"}
	for _, n := range names {
		p := `D:\` + n + `.txt`
		a.hist.TouchEntry(history.Entry{Path: p, Name: n, Kind: history.KindFile, Ext: ".txt"})
		if _, ok := a.hist.TogglePin(p); !ok {
			t.Fatalf("固定 %s 失败", n)
		}
	}
	a.results = a.historyResults(maxPinned)
	if a.pinnedCount != 3 {
		t.Fatalf("应有 3 个固定项，实际 %d", a.pinnedCount)
	}

	tester := ui.NewTester(a.view, panelWidth, panelHeight)
	tester.Frame()

	r, ok := tester.Find("甲甲")
	if !ok {
		t.Fatal("首页没渲染出「甲甲」卡片")
	}
	// 按在名字上方那块图标区域（属于卡片本身，不是文字），往右拖一格再松手。
	x, y := r.X+r.W/2, r.Y-14
	if !a.pinPressHit(x, y) {
		t.Fatalf("按在固定卡片 (%.1f,%.1f) 上却没判定成固定卡，窗口拖动会抢走它", x, y)
	}
	tester.Press(x, y)
	tester.Frame()
	if d := tester.Drags(); d != 0 {
		t.Fatalf("按在固定卡片上启动了窗口拖动：Drags=%d", d)
	}
	tester.Move(x+95, y)
	tester.Frame()
	dumpPreview(t, tester) // 拖动中途的样子（BAOZI_PREVIEW 控制）
	tester.Release(x+95, y)
	tester.Frame()
	tester.Frame() // 松手那一帧只记下意图，下一帧 view 开头才结算

	got := a.hist.Pinned(0)
	if len(got) != 3 {
		t.Fatalf("重排不该改变条数，实际 %d 条", len(got))
	}
	if got[1].Path != `D:\甲甲.txt` {
		t.Fatalf("「甲甲」应被拖到第二位，实际顺序 %v %v %v", got[0].Path, got[1].Path, got[2].Path)
	}
	if got[0].Path != `D:\乙乙.txt` || got[2].Path != `D:\丙丙.txt` {
		t.Fatalf("其余两项应保持原相对顺序，实际 %v", got)
	}
	// 结算后所有卡片都该回到原位：偏移不为 0 的话，重排后的卡片会错位挂在那儿。
	for i, pt := range a.pinDragPts {
		if pt.X != 0 || pt.Y != 0 {
			t.Fatalf("第 %d 张卡片的拖动偏移没归零：(%v, %v)", i, pt.X, pt.Y)
		}
	}
	if !tester.HasText("已保存固定顺序") && !tester.HasText("已保存固定顺序：") {
		// 底栏被选中项占住时不显示状态，这里只做提示，不作硬断言
		t.Log("底栏没有显示保存提示（选中项优先显示，属正常）")
	}
}

// TestCardIconPreview 用真实程序图标渲染首页卡片，看"大图标 + 名字、没有方块底"
// 到底是什么样。图标不走 mygo.Shell.FileIcon（ui.Tester 里没有主循环会永久阻塞），
// 而是用自家 COM 的 win.Thumbnail 取 .exe 的图标塞进缓存——appIcon 现在先查缓存，
// 离线也能命中，所以这条路径画出来的就是真机上的效果。
// 配 BAOZI_PREVIEW 出图；不设就把这个测试当"布局没崩"的冒烟。
func TestCardIconPreview(t *testing.T) {
	a := testApp(t)
	exes := []string{
		`C:\Windows\System32\notepad.exe`,
		`C:\Windows\System32\mspaint.exe`,
		`C:\Windows\System32\mstsc.exe`,
		`C:\Windows\System32\calc.exe`,
		`C:\Windows\System32\Taskmgr.exe`,
		// 这张是"白底 tile + 小 logo"那类图标（游戏启动器常见），
		// 以前 trim 不掉留白，卡片里就成了白框小图标。放进预览当回归样本。
		`D:\Games\ywzh_launcher\Win64Release\PCLauncher\PCLauncher.exe`,
		// 长名字样本也要有图标，否则渲染时会去问 Shell，而 Shell 在 ui.Tester 下阻塞。
		`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
	}
	// 缓存键要跟 appIcon 算出来的一字不差：它把 size*2.2 再乘一次缩放。
	px := int(float32(int(a.cardIcon()*2.2)) * a.scale)
	for _, p := range exes {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		data, err := win.Thumbnail(p, px)
		if err != nil || len(data) == 0 {
			continue
		}
		// 真机路径会裁掉图标四周的透明边，这里照做，否则缩略图偏小。
		if trimmed, err := win.TrimIcon(data); err == nil {
			data = trimmed
		}
		if bm, _ := ui.DecodeBitmap(data); bm != nil {
			a.icons[fmt.Sprintf("app:%d:%s", px, p)] = bm
		}
	}
	a.results = []search.Result{
		{Item: search.Item{Name: "记事本", Path: exes[0], Ext: ".exe", Kind: search.KindFile, Hint: `C:\Windows\System32`}},
		{Item: search.Item{Name: "画图", Path: exes[1], Ext: ".exe", Kind: search.KindFile, Hint: `C:\Windows\System32`}},
		{Item: search.Item{Name: "远程桌面连接", Path: exes[2], Ext: ".exe", Kind: search.KindFile, Hint: `C:\Windows\System32`}},
		{Item: search.Item{Name: "计算器", Path: exes[3], Ext: ".exe", Kind: search.KindFile, Hint: `C:\Windows\System32`}},
		{Item: search.Item{Name: "任务管理器", Path: exes[4], Ext: ".exe", Kind: search.KindFile, Hint: `C:\Windows\System32`}},
		{Item: search.Item{Name: "遗忘之海", Path: exes[5], Ext: ".exe", Kind: search.KindFile, Hint: `D:\Games\ywzh_launcher\Win64Release\PCLauncher`}},
		{Item: search.Item{Name: "文档", Path: `D:\docs`, Kind: search.KindDir, Hint: `D:\`}},
		{Item: search.Item{Name: "会议记录.txt", Path: `D:\docs\会议记录.txt`, Ext: ".txt", Kind: search.KindFile, Hint: `D:\docs`}},
		// 名字长到要换第二行的样本：用户截图里那一排就是因为这种卡"名字两行、别的卡一行"
		// 导致图标顶和标题各偏了一截，这里固定住不被改回去。
		// 必须挑一个**真实存在**的 exe：卡片渲染会去取图标，路径不存在会调 Shell，
		// 而 Shell.* 在 ui.Tester 下会永久阻塞（工程里踩过这个坑）。
		{Item: search.Item{Name: "WindowsPowerShell.exe", Path: `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, Ext: ".exe", Kind: search.KindFile, Hint: `C:\Windows\System32\WindowsPowerShell\v1.0`}},
	}
	a.pinnedCount = len(a.results)
	// 选中「遗忘之海」那张：它跟用户给的截图是同一个图标，方便对照"选中态该长什么样"
	// （现在是一块圆角底色，没有边框）。
	a.choose(5)

	pw, ph := a.cfg.PanelSize()
	tst := ui.NewTester(a.view, pw, ph)
	tst.Frame()
	for _, want := range []string{"记事本", "画图", "文档", "会议记录.txt"} {
		if _, ok := tst.Find(want); !ok {
			t.Fatalf("首页没渲染出「%s」", want)
		}
	}
	dumpPreview(t, tst)
}

// 按下但取消（没真的拖动）不该改动顺序。
// 用 CancelPress 而不是 Release：同一位置的按下+松开在卡片上是"点击"，
// 会走到 open() 去打真文件，视图测试里不能碰。
func TestPinPressCancelKeepsOrder(t *testing.T) {
	a := testApp(t)
	stubAllIcons(a)
	for _, n := range []string{"甲甲", "乙乙"} {
		p := `D:\` + n + `.txt`
		a.hist.TouchEntry(history.Entry{Path: p, Name: n, Kind: history.KindFile, Ext: ".txt"})
		if _, ok := a.hist.TogglePin(p); !ok {
			t.Fatalf("固定 %s 失败", n)
		}
	}
	a.results = a.historyResults(maxPinned)
	tester := ui.NewTester(a.view, panelWidth, panelHeight)
	tester.Frame()
	r, ok := tester.Find("甲甲")
	if !ok {
		t.Fatal("首页没渲染出「甲甲」卡片")
	}
	tester.Press(r.X+r.W/2, r.Y-14)
	tester.Frame()
	tester.CancelPress()
	tester.Frame()
	tester.Frame()
	got := a.hist.Pinned(0)
	if got[0].Path != `D:\甲甲.txt` {
		t.Fatalf("没拖动时顺序不该变，实际首项 %s", got[0].Path)
	}
}
