package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// 竖滚动条的可按下区域：贴着容器右边缘、宽 ScrollbarWidth+6，高度整条。
// 这个数必须和 mygo 的 scrollBars（paint.go）一致 —— 差几像素的后果是
// "按在条上没生效"或者"按在内容上却被判成滚动条、面板拖不动"。
func TestListBarStrip(t *testing.T) {
	theme := &ui.Theme{} // ScrollbarWidth 未设 → 走默认 6
	box := ui.Rect{X: 10, Y: 20, W: 300, H: 400}
	got := listBarStrip(box, theme)
	want := ui.Rect{X: 10 + 300 - 12, Y: 20, W: 12, H: 400}
	if got != want {
		t.Fatalf("默认主题的滚动条区域 = %+v，期望 %+v", got, want)
	}

	// 主题把拇指调宽了，可按下区域跟着变宽
	theme.ScrollbarWidth = 10
	got = listBarStrip(box, theme)
	want = ui.Rect{X: 10 + 300 - 16, Y: 20, W: 16, H: 400}
	if got != want {
		t.Fatalf("宽度 10 时滚动条区域 = %+v，期望 %+v", got, want)
	}

	// 列表还没提交过一帧：不能给一个假矩形
	if got := listBarStrip(ui.Rect{}, theme); got != (ui.Rect{}) {
		t.Fatalf("空盒子应当返回零矩形，实际 %+v", got)
	}
	if got := listBarStrip(box, nil); got != (ui.Rect{}) {
		t.Fatalf("没有主题应当返回零矩形，实际 %+v", got)
	}
}

// 按下命中判定：条内算、条右边界之外算、没有条时一律不算。
func TestScrollBarPressHit(t *testing.T) {
	a := testApp(t)
	if a.scrollBarPressHit(10, 10) {
		t.Fatal("还没记下滚动条时不该命中")
	}
	a.listBar = ui.Rect{X: 300, Y: 100, W: 12, H: 200}

	cases := []struct {
		x, y float32
		want bool
	}{
		{306, 200, true},
		{300, 100, true}, // 左上角含
		{311.5, 299.5, true},
		{312, 200, false}, // 右边界外：半开区间
		{299, 200, false},
		{306, 99, false},
		{306, 300, false},
	}
	for _, c := range cases {
		if got := a.scrollBarPressHit(c.x, c.y); got != c.want {
			t.Fatalf("(%v, %v) 命中 = %v，期望 %v", c.x, c.y, got, c.want)
		}
	}
}

// 端到端：列表页渲染一帧之后要把滚动条区域记下来；回到首页要清掉。
// 清了这一格很重要 —— 首页那几条卡片路径上没有列表，留着旧矩形会把
// 空白处的按下误判成"按在滚动条上"，面板在那儿就拖不动了。
func TestResultListRecordsBarRect(t *testing.T) {
	a := testApp(t)
	stubAllIcons(a)
	a.query = "pdf"
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame() // 第一帧只建列表，Bounds 还没有值
	tr.Frame() // 这一帧才读到上一帧提交的矩形
	if a.listBar.W <= 0 || a.listBar.H <= 0 {
		t.Fatalf("列表页没有记下滚动条区域：%+v", a.listBar)
	}
	// 必须是列表右边缘那一条：宽度 12（默认拇指 6 + 轨道左右各 3），
	// 高度占满可视区，横向落在面板右半区。
	if a.listBar.W != 12 {
		t.Fatalf("滚动条宽度 = %v，期望 12", a.listBar.W)
	}
	if a.listBar.X < panelWidth/2 || a.listBar.X+a.listBar.W > panelWidth {
		t.Fatalf("滚动条区域横向位置不对：%+v（面板宽 %v）", a.listBar, panelWidth)
	}
	if a.listBar.H < panelHeight/4 {
		t.Fatalf("滚动条区域高度只有 %v，不像是整条：%+v", a.listBar.H, a.listBar)
	}

	a.query = ""
	tr.Frame()
	if a.listBar != (ui.Rect{}) {
		t.Fatalf("回到首页后滚动条区域应当清空，实际 %+v", a.listBar)
	}
}
