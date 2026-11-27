package main

// 自绘拖动：把面板的拖动从系统的模态拖动循环里摘出来。
//
// 为什么必须这么做：系统的拖动（mygo 的 DragWindow = ReleaseCapture +
// SendMessage(WM_NCLBUTTONDOWN, HTCAPTION)）由 Windows 自己处理鼠标。窗口位置是
// 系统**立即**设的，玻璃内容却要等我们算完 → 提交 → DWM 合成，实测帧周期
// 24~36ms（每帧 RedrawWindow 都在等 vsync）。错位量 = 拖动速度 × 帧周期，
// 快拖时十几像素，一眼就看出来。
//
// 自己接管以后顺序可以反过来：**先把内容画好、再挪窗口、再 Present**，让
// "窗口新位置"和"新内容"落在同一次合成里 —— 相对位移为 0，怎么快都不错位。
//
// 为什么要用 SetWindowSubclass 而不是 SetWindowLongPtr 换 WndProc：
// mygo 在每次 Show / 改窗口样式时都可能再 SetWindowLongPtr 一次，直接把换上去的
// proc 顶掉（第一版就是这样，装上以后一段时间就静默失效，表现是"完全拖不动"）。
// 子类化是插在消息链**最前面**的一层，mygo 换 GWLP_WNDPROC 也摘不掉它。
//
// 消息处理上的取舍：
//   · 左键按下照旧转发给 mygo（图标、搜索框、底部按钮的按下态和聚焦都保持原样）；
//   · 位移超过 6px 才开始接管，之前照旧转发（单击、拖选不受影响）；
//   · 接管之后 move 不再转发（免得 mygo 还在做 hover），up 也不转发
//     （否则会把"在拖动终点的那个图标"当成点击打开）；
//   · 于是 mygo 认为鼠标还按着 —— 松手后它会等下一次按下自己复位，视觉上
//     最坏是某个按钮保持按下色，不影响使用。

import (
	"log"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"baozi/internal/win"
)

var (
	dragUser32 = windows.NewLazySystemDLL("user32.dll")
	dragCtl32  = windows.NewLazySystemDLL("comctl32.dll")

	procSetSubclass   = dragCtl32.NewProc("SetWindowSubclass")
	procDelSubclass   = dragCtl32.NewProc("RemoveWindowSubclass")
	procDefSubclass   = dragCtl32.NewProc("DefSubclassProc")
	procEnumChild     = dragUser32.NewProc("EnumChildWindows")
	procGetClassNameW = dragUser32.NewProc("GetClassNameW")

	procSetCapture = dragUser32.NewProc("SetCapture")
	procRelCapture = dragUser32.NewProc("ReleaseCapture")
	procGetCursor  = dragUser32.NewProc("GetCursorPos")
	procSetWinPos  = dragUser32.NewProc("SetWindowPos")
	procScreenCli  = dragUser32.NewProc("ScreenToClient")
	procGetDpi     = dragUser32.NewProc("GetDpiForWindow")

	// 问"某个键现在还按着吗"：捕获丢了要靠它分辨"拖完了"还是"被抢了"。
	procGetAsyncKey = dragUser32.NewProc("GetAsyncKeyState")
)

const (
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmMouseMove   = 0x0200
	wmCancelMode  = 0x001F

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpShowWindow = 0x0040

	// 开始接管的位移阈值（物理像素）。小于它是普通点击/拖选，照旧交给 mygo。
	dragThreshold = 6

	// 子类化用的固定 id，一个窗口只挂一份。
	dragSubclassID = 0x626F7A // "boz"
)

type point struct{ X, Y int32 }

// selfDrag 是面板自绘拖动的状态机。
type selfDrag struct {
	app   *application
	host  uintptr // 顶层窗口：SetWindowPos / 取客户区原点 / 重绘的目标
	hwnd  uintptr // 挂子类化的窗口：mygo 的 MyGoSurface 子窗口 = 鼠标消息真正落点
	tramp uintptr

	pending bool // 按下但还没到阈值
	active  bool // 已接管

	startCursorX, startCursorY int32
	startWinX, startWinY       int32
	lastX, lastY               int32

	frames int
	t0     time.Time

	// clientDX/clientDY 是客户区相对窗口左上角的偏移。窗口有 THICKFRAME
	// （那圈看不见的缩放边框），客户区原点不等于窗口原点；底图取景用的是**客户区**
	// 原点，直接用 SetWindowPos 的 x,y 会让整张玻璃偏出几个像素。
	clientDX, clientDY int32

	// lastDraw 是拖动渲染的节流点：SetWindowPos 会反过来再派 WM_MOUSEMOVE，
	// 不限速时每次移动都能滚出一串几百帧（实测 405fps），白烧 CPU。
	lastDraw time.Time

	// 分项耗时累计（微秒）：挪窗口 / 阴影 / 算底图 / 排帧。拖动里到底哪一步在
	// 吃掉帧时间，只能靠这个数出来。
	costPos, costShadow, costBacking, costInvalidate int64
	costSamples                                      int64

	// faded 表示拖动中已经切到糊化玻璃（setDragFade），松手时恢复清晰。
	faded bool

	dbgLog int
}

// findMyGoSurface 在顶层窗口下面找 mygo 的 MyGoSurface 子窗口 —— 鼠标消息落点。
// surface.go 里它是 createWindow(wsExNoRedirect, surfaceClass, ..., w.hwnd) 建的
// 子窗口：真正接鼠标的是它，不是顶层 MyGoWindow。
func findMyGoSurface(host uintptr) uintptr {
	var found uintptr
	cb := windows.NewCallback(func(child, _ uintptr) uintptr {
		var buf [64]uint16
		procGetClassNameW.Call(child, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if windows.UTF16ToString(buf[:]) == "MyGoSurface" {
			found = child
			return 0
		}
		return 1
	})
	procEnumChild.Call(host, cb, 0)
	return found
}

// installSelfDrag 在面板窗口上挂一层子类化。必须在窗口建好之后调用，可重复调用。
//
// 注意挂的**不是**顶层窗口：mygo 的鼠标输入走 MyGoSurface 这个子窗口，消息根本
// 不会经过顶层窗口的消息过程（第一版挂错地方，结果一条鼠标消息都收不到，
// 表现就是"完全拖不动"）。
func (a *application) installSelfDrag(host uintptr) {
	if host == 0 {
		return
	}
	if a.selfDrag != nil && a.selfDrag.host == host && a.selfDrag.hwnd != 0 {
		return
	}
	target := findMyGoSurface(host)
	if target == 0 {
		// 子窗口还没建出来。mygo 的 MyGoSurface 是 Show 之后才 createWindow 的，
		// 时序偶尔差一拍（实测有过一次：日志只留一句"子类化没挂上"，用户那边
		// 就是"又拖不动了"）。
		//
		// 这里**不能**退到顶层窗口上挂：顶层收不到鼠标消息（第一条就是栽在这上面），
		// 挂了也拖不动，还会把"再来一次"的机会吃掉。改成隔 50ms 再试几拍。
		if a.dragRetryLeft > 0 && a.panel != nil {
			a.dragRetryLeft--
			time.AfterFunc(50*time.Millisecond, func() {
				a.panel.Update(func() {
					if h := a.panel.NativeHandle(); h != 0 {
						a.installSelfDrag(h)
					}
				})
			})
			return
		}
		log.Printf("拖动: 找不到 MyGoSurface（host=%d），自绘拖动没挂上", host)
		return
	}
	if a.selfDrag != nil && a.selfDrag.hwnd != 0 {
		a.selfDrag.detach()
	}
	d := &selfDrag{app: a, host: host, hwnd: target}
	d.tramp = windows.NewCallback(d.subclassProc)
	if r, _, err := procSetSubclass.Call(target, d.tramp, dragSubclassID, 0); r == 0 {
		log.Printf("拖动: 子类化没挂上（%v），下一拍再试", err)
		if a.dragRetryLeft > 0 && a.panel != nil {
			a.dragRetryLeft--
			time.AfterFunc(50*time.Millisecond, func() {
				a.panel.Update(func() { a.installSelfDrag(host) })
			})
		}
		return
	}
	a.dragRetryLeft = 8
	a.selfDrag = d
	log.Printf("拖动: 已接管（host=%d surface=%d，位移超过 %d 像素才真正开始拖）",
		host, target, dragThreshold)
}

// raise 把面板提到置顶组的最前面。它不是"永远最前"，只是保证唤出后点得到：
// 别的置顶窗口（比如豆包工作窗）盖在上面时，鼠标消息会落到它那儿，面板就
// "拖不动"了 —— 这正是这一版第一次部署后用户遇到的现象。
func (a *application) raisePanel() {
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	procSetWinPos.Call(hwnd, ^uintptr(0), 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoActivate)
}

func (d *selfDrag) detach() {
	if d.hwnd != 0 {
		procDelSubclass.Call(d.hwnd, d.tramp, dragSubclassID)
		d.hwnd = 0
	}
}

func cursorPos() (int32, int32) {
	var p point
	procGetCursor.Call(uintptr(unsafe.Pointer(&p)))
	return p.X, p.Y
}

// windowScale 返回窗口所在显示器的缩放：UI 按 DIP 排版，客户区消息是物理像素。
// 125% 屏上卡片矩形是 846 宽，客户区却是 1057 宽，比较前必须除回 DIP。
func windowScale(hwnd uintptr) float32 {
	dpi, _, _ := procGetDpi.Call(hwnd)
	if dpi == 0 {
		return 1
	}
	return float32(dpi) / 96
}

func windowXY(hwnd uintptr) (int32, int32) {
	x, y, _, _ := win.WindowRectPixels(hwnd)
	return int32(x), int32(y)
}

func setWindowXY(hwnd uintptr, x, y int32) {
	// SWP_NOZORDER：不带它每次移动都会把这个 topmost 窗口重新提到置顶组最前，
	// 而重排 z 序要牵动整个桌面重新合成 —— 拖动里每帧一次，就是"拖一下卡一下"。
	procSetWinPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0,
		swpNoSize|swpNoZOrder|swpNoActivate)
}

// measureClientOffset 记下客户区相对窗口左上角的偏移（见 clientDX 的说明）。
func (d *selfDrag) measureClientOffset() {
	wx, wy, _, _ := win.WindowRectPixels(d.host)
	cx, cy, _, _ := win.ClientRectPixels(d.host)
	d.clientDX, d.clientDY = int32(cx-wx), int32(cy-wy)
}

func (d *selfDrag) subclassProc(hwnd, msg, wp, lp, id, ref uintptr) uintptr {
	if (msg == wmLButtonDown || msg == wmLButtonUp || msg == wmMouseMove) && d.dbgLog < 30 {
		d.dbgLog++
		cx, cy := cursorPos()
		log.Printf("拖动: 消息 0x%04x active=%v pending=%v 光标 %d,%d",
			msg, d.active, d.pending, cx, cy)
	}
	switch msg {
	case wmLButtonDown:
		d.pending = true
		d.active = false
		d.frames = 0
		d.t0 = time.Now()
		d.startCursorX, d.startCursorY = cursorPos()
		d.startWinX, d.startWinY = windowXY(d.host)
		d.lastX, d.lastY = d.startWinX, d.startWinY
		// 固定卡片自己会处理拖动排序。窗口拖动必须在这里就让开，否则子类化
		// 比 mygo 先收到鼠标移动，会把卡片拖动直接抢成拖整块面板。
		var pt point
		pt.X, pt.Y = d.startCursorX, d.startCursorY
		procScreenCli.Call(hwnd, uintptr(unsafe.Pointer(&pt)))
		scale := windowScale(hwnd)
		hit := d.app.pinPressHit(float32(pt.X)/scale, float32(pt.Y)/scale)
		if hit {
			d.pending = false
		}
		// 滚动条同理：按在结果列表的竖条上，这次按住属于 mygo 的滚动条拖动，
		// 不能顺手把面板也拖走（用户报的就是"拖滚动条时整个窗口跟着动"）。
		if d.app.scrollBarPressHit(float32(pt.X)/scale, float32(pt.Y)/scale) {
			d.pending = false
		}
		// 照旧转发：图标/搜索框/按钮的按下态、聚焦都要走 mygo 那一套。
		return d.call(hwnd, msg, wp, lp)

	case wmMouseMove:
		if d.active {
			d.dragTo()
			return 0
		}
		if d.pending {
			cx, cy := cursorPos()
			dx, dy := cx-d.startCursorX, cy-d.startCursorY
			if dx > dragThreshold || dx < -dragThreshold || dy > dragThreshold || dy < -dragThreshold {
				if r, _, _ := procSetCapture.Call(hwnd); r == 0 {
					return 0
				}
				d.active = true
				d.measureClientOffset()
				d.dragTo()
				return 0
			}
		}
		return d.call(hwnd, msg, wp, lp)

	case wmLButtonUp:
		was := d.active
		d.pending = false
		d.active = false
		if was {
			// 补最后一拍位置，恢复清晰玻璃，然后按新位置重算一次取景并请求一帧。
			d.dragTo()
			d.app.setDragFade(false)
			d.faded = false
			d.app.refreshBacking(true)
			if d.app.panel != nil {
				d.app.panel.Invalidate()
			}
			d.app.followShadow()
		}
		// 只在**真的拖过**的时候松捕获。早先这里无条件 ReleaseCapture，
		// 结果面板里所有"按下-抬起"配对的控件（设置/互传按钮、结果行点击）
		// 全哑了 —— mygo 的点击靠 SetCapture 跟踪，中途被我们收回去就断了。
		if was {
			procRelCapture.Call()
		}
		if was {
			if n := d.frames; n > 1 {
				if el := time.Since(d.t0); el > 0 {
					log.Printf("拖动: 自绘拖动 %d 帧 / %dms = %.0f fps",
						n, el.Milliseconds(), float64(n)/el.Seconds())
				}
			}
			d.app.panelGeometryChanged()
			// 不转发 up：否则 mygo 会把"拖动终点"下的那个图标当点击。
			return 0
		}
		return d.call(hwnd, msg, wp, lp)

	case wmCancelMode:
		if d.active || d.pending {
			d.pending = false
			d.active = false
			procRelCapture.Call()
		}
		return d.call(hwnd, msg, wp, lp)
	}
	return d.call(hwnd, msg, wp, lp)
}

func (d *selfDrag) call(hwnd, msg, wp, lp uintptr) uintptr {
	ret, _, _ := procDefSubclass.Call(hwnd, msg, wp, lp)
	return ret
}

// dragTo 的顺序是刻意的，也是这整件事的答案：
//
//  1. 先 SetWindowPos —— 这一刻起窗口就贴在光标上了（零点几毫秒）。以前把
//     算底图放在前面，窗口要等算完才动，鼠标一快就是"追不上光标"。
//  2. 阴影跟着挪（Nudge，只 SetWindowPos，不重传位图）。
//  3. 最后才补内容：平移底图（memmove + 从快照补新露出的带，1~2ms）+ 排一帧。
//     这两步按 16ms 节流，而且**不** RedrawNow（那会同步等一次 vsync，把消息
//     处理卡成每帧 16ms）。所以窗口每条消息都跟手，背景最多落后一帧。
func (d *selfDrag) dragTo() {
	cx, cy := cursorPos()
	nx := d.startWinX + (cx - d.startCursorX)
	ny := d.startWinY + (cy - d.startCursorY)
	if nx == d.lastX && ny == d.lastY {
		return
	}
	d.lastX, d.lastY = nx, ny

	// 0) 拖动第一帧：把玻璃糊化一次（重做磨砂图 + 整张重烘，十毫秒级，只做一次）。
	//    松手时 up 里再恢复清晰。见 glass.go 的 setDragFade。
	if !d.faded {
		d.faded = true
		d.app.setDragFade(true)
	}

	// 1) 先把内容算好（平移底图，1~2ms）。窗口还在旧位置，但纹理里已经是
	//    新位置的取景。
	tBake := time.Now()
	d.app.refreshBackingFor(int(nx)+int(d.clientDX), int(ny)+int(d.clientDY), false)
	d.costBacking += int64(time.Since(tBake))

	// 2) 挪窗口。这一步起窗口就贴着光标了。
	tPos := time.Now()
	setWindowXY(d.host, nx, ny)
	d.costPos += int64(time.Since(tPos))

	// 3) 阴影跟着走（只挪位置，不重传位图 —— UpdateLayeredWindow 每帧整张提交
	//    一千多像素宽的位图，十几毫秒）。
	tShadow := time.Now()
	d.app.followShadowNudge()
	d.costShadow += int64(time.Since(tShadow))

	// 4) 立刻把这一帧画出来并提交。**这是关键**：mygo 平时走 paced 帧，画完要
	//    等下一次 DWM 刷新（DwmFlush）才提交，于是"窗口已经在新位置、内容还是
	//    旧的"要持续一整帧；FrameNow 不等刷新，位置和内容一起进同一个合成帧。
	tFrame := time.Now()
	if d.app.panel != nil {
		d.app.panel.FrameNow()
	}
	d.costInvalidate += int64(time.Since(tFrame))
	d.costSamples++

	d.app.uiTick.Store(time.Now().UnixNano())
	d.app.dragUntil.Store(time.Now().Add(600 * time.Millisecond).UnixNano())
	d.frames++
	if d.frames%30 == 0 {
		n := 30.0
		log.Printf("拖动: setpos %.2fms 阴影 %.2fms 底图 %.2fms 排帧 %.2fms",
			float64(d.costPos)/n/1000, float64(d.costShadow)/n/1000,
			float64(d.costBacking)/n/1000, float64(d.costInvalidate)/n/1000)
		d.costPos, d.costShadow, d.costBacking, d.costInvalidate = 0, 0, 0, 0
	}
}
