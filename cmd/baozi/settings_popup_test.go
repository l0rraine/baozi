package main

import (
	"image"
	"testing"

	"github.com/egoist/mygo/ui"
)

// 下拉弹层的底板必须是不透明的。
//
// 设置窗是整窗自绘的玻璃：Theme.Background 被设成真透明（圆角裁掉的四角要露桌面），
// 而弹层照着 Theme.Background 画，弹出来就是一层"能看穿的菜单"——底下的复选框、
// 滑杆、下一个分区标题全压在选项上（用户报的就是"下拉以后全部都是透明的"）。
func TestSelectPopupIsOpaque(t *testing.T) {
	a := testApp(t)
	// 只有 frost 不为空才走"玻璃底"那一路，主题才会被换成透明背景。
	a.frost = ui.NewBitmap(image.NewRGBA(image.Rect(0, 0, 32, 32)))
	s := &settingsWindow{app: a}
	s.load()
	tr := ui.NewTester(s.view, 880, 900)
	tr.Frame()

	value, ok := tr.Find("100%")
	if !ok {
		t.Fatal("找不到图标大小的下拉框")
	}
	tr.ClickAt(value.X+value.W/2, value.Y+value.H/2)
	tr.Frame()
	tr.Frame()

	opt, ok := tr.Find("175%")
	if !ok {
		t.Fatal("下拉没有弹出来")
	}
	img := tr.Image()
	// 取样点落在选项行里、文字右边：那里只有弹层底板和这一行的填充。
	x, y := int(opt.X+opt.W+12), int(opt.Y+opt.H/2)
	c := img.RGBAAt(x, y)
	if c.A != 255 {
		t.Fatalf("弹层底板是透明的（alpha=%d）：底下那层内容会整个透上来", c.A)
	}
	dumpPreview(t, tr)
}
