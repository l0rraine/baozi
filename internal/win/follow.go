// 读文件管理器当前目录 + 文件对话框自动跟随。
//
// 对应 Lertaro 的 `IActivePathCollector`（各文件管理器一个 collector）与
// `Core/Hook/ExplorerIntegration/FileDialogNavigationTracker.cs`：
//
//  1. 记住"最近一次文件管理器所在的目录"（lastDir）；目录一变版本号 +1，
//     另外只要文件管理器窗口重新成为前台也 +1（换没换目录都算"去看过"）；
//     目录来源目前接了七种（见 sources.go 的 Source*）：
//     - 资源管理器（CabinetWClass，读地址栏面包屑 + 已知文件夹名字还原）
//     - Total Commander（TTOTAL_CMD，WM_COPYDATA 远程控制协议，totalcmd.go）
//     - Directory Opus（dopus.lister，文件显示容器的地址栏，opus.go）
//     - XYplorer（ThunderRT6FormDC，脚本协议问 <curpath>，xyplorer.go）
//     - xplorer2（ATL:ExplorerFrame，读可见的地址栏 ComboBox，xplorer2.go）
//     - tessoa（Rust+wgpu 自绘，UIA 树只有顶层一个节点，只能调它的
//     `--print-cwd` 问活动窗格，tessoa.go）
//     - Arke（Rust+GPUI 双栏，调它的 `--api -` state 接口问活动窗格，arke.go）
//     每种都能在设置里单独关掉。
//  2. 每当一个「打开 / 另存为」框成为前台：
//     - 第一次见到它：只有上一次活动的确实是文件管理器，才把最近记住的目录送过去；
//     - 之后它再出现、而版本号变过（期间切到过文件管理器，换没换目录都算）→ 再送一次，
//     不管中间是否还夹了别的窗口（Alt+Tab 会先切到应用主窗，见 shouldFollow）；
//     - 版本没变就一动不动，免得把用户手动在框里选的目录拽回去。
//
// 于是用户的实际感受就是：在文件管理器里按"打开 / 另存为"，框直接落在他刚才看的位置。
// Lertaro 那条"上一次激活的确实是文件管理器才重读"对**第一次出现**仍然成立，
// 防止从任何程序打开的框被一个旧目录拽走；但同一个框第二次以后靠版本号判断，
// 否则 Alt+Tab 回来（中间夹了应用主窗）就会漏。
package win

import (
	"context"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetAncestor = user32.NewProc("GetAncestor")
)

const (
	gaRootOwner      = 2
	explorerPollTick = 200 * time.Millisecond
	// tessoaTitleInterval 是"读活动窗格标题"的最小间隔。
	// 读标题要往 tessoa 发 WM_GETTEXT，人眼跟不上更快的变化，500ms 够用。
	tessoaTitleInterval = 500 * time.Millisecond
)

// RootOwner 沿 GA_ROOTOWNER 上溯：多标签资源管理器里前台常是 ShellTabWindowClass 这类子窗口，
// 上溯到 CabinetWClass 才能读地址栏。
func RootOwner(hwnd uintptr) uintptr {
	if hwnd == 0 {
		return 0
	}
	if root, _, _ := procGetAncestor.Call(hwnd, gaRootOwner); root != 0 {
		return root
	}
	return hwnd
}

// IsExplorer 判断是不是资源管理器窗口（单窗口 CabinetWClass，老式 ExploreWClass）。
func IsExplorer(root uintptr) bool {
	cls := className(root)
	return strings.EqualFold(cls, "CabinetWClass") || strings.EqualFold(cls, "ExploreWClass")
}

// isPathSource 判断这个窗口能不能提供"当前目录"。
// 六种来源见 sources.go；每种都可以在设置里单独关掉（关掉后连读都不读）。
func (f *DialogFollow) isPathSource(hwnd uintptr) bool {
	kind := sourceKind(hwnd)
	return kind != "" && f.sourceEnabled(kind)
}

// folderOf 按窗口类型选对应的读法。
func (f *DialogFollow) folderOf(hwnd uintptr) string {
	kind := sourceKind(hwnd)
	if kind == "" || !f.sourceEnabled(kind) {
		return ""
	}
	return sourceFolder(kind, hwnd)
}

// folderOfFresh 同上，但要求现场重读（tessoa 会因此多起一个进程，Arke 多查一次 state）。
func (f *DialogFollow) folderOfFresh(hwnd uintptr) string {
	kind := sourceKind(hwnd)
	if kind == "" || !f.sourceEnabled(kind) {
		return ""
	}
	return sourceFolderFresh(kind, hwnd)
}

// sourceEnabled 问某个来源是否启用；拼 follow 时没给 enabled 就一律启用。
func (f *DialogFollow) sourceEnabled(kind string) bool {
	if f.enabled == nil {
		return true
	}
	return f.enabled(kind)
}

// ExplorerFolder 读资源管理器当前目录。
// 地址栏和文件对话框是同一套控件（Breadcrumb Parent → ToolbarWindow32），
// 那个工具条的 window text 就是完整路径，形如 "地址: D:\work\baozi"。
// 但在「下载 / 文档 / 音乐」这类已知文件夹的根上，地址栏给的是**本地化名字**
// （实测 "地址: 音乐"），所以统一过一遍 resolveFolderText 还原成真实路径。
func ExplorerFolder(root uintptr) string {
	bread := findChildByClass(root, "Breadcrumb Parent")
	if bread == 0 {
		return ""
	}
	var toolbar uintptr
	// 直接子层只有一个 id=1001 的 ToolbarWindow32；用递归枚举以免以后换了层级。
	cb := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
		if toolbar != 0 {
			return 1
		}
		if strings.EqualFold(className(c), "ToolbarWindow32") {
			parent, _, _ := procGetParent.Call(c)
			if strings.EqualFold(className(parent), "Breadcrumb Parent") {
				toolbar = c
				return 0
			}
		}
		return 1
	})
	procEnumChildWindows.Call(bread, cb, 0)
	if toolbar == 0 {
		return ""
	}
	return resolveFolderText(WindowTextTimeout(toolbar))
}

// WindowTextTimeout 带超时读文本：目标进程卡住时不把调用方一起拖死
// （资源管理器在大目录 / 网络盘上确实会卡）。
func WindowTextTimeout(hwnd uintptr) string {
	buf := make([]uint16, maxText)
	sendMsg(hwnd, wmGetText, maxText, uintptr(unsafe.Pointer(&buf[0])))
	return windows.UTF16ToString(buf)
}

// FollowEvent 是一次"把对话框送到某目录"。
type FollowEvent struct {
	HWND uintptr
	Dir  string
}

// DialogFollow 复刻 Lertaro 的 FileDialogNavigationTracker。
//
// 关键一点（Lertaro 源码里那行 `previousWasPathProvider && readProviderPath()` 的含义）：
// 轮询期间读到的目录可能已经过时 —— 用户在资源管理器里换目录的那一刻不一定被采到。
// 所以**对话框重新成为前台时，要现场再读一次"上一次活动的那个资源管理器窗口"的目录**。
// 只记 hwnd 不记内容，读的时刻永远是最新的，这比"轮询时顺手读一次再存起来"稳得多。
// 反过来，如果上一次活动的是别的程序，就不重读（否则用户手动在框里换的目录会被拽回旧目录）。
type DialogFollow struct {
	mu      sync.Mutex
	lastDir string
	version int
	seen    map[uintptr]int
	lastFg  uintptr

	// lastActive 是上一次观察到的前台窗口根（用来判断"上一次活动的是不是文件管理器"）。
	// lastSource 是最近一次活动的那个文件管理器窗口（资源管理器 / Total Commander）。
	lastActive uintptr
	lastSource uintptr
	// lastTitle 是最后看过的那个来源窗口的标题。tessoa 换窗格时前台窗口不变，
	// 只能拿标题（它带着活动目录名）当变更信号。
	lastTitle   string
	lastTitleAt time.Time

	onNavigate func(FollowEvent)
	dbg        func(format string, args ...any)
	// enabled 回答"某个来源现在还开不开"。设置在运行中被改时，跟随器下一次轮询
	// 就能看到新值，不需要重启。
	enabled func(kind string) bool
}

// NewDialogFollow 建一个跟随器。onNavigate 在不阻塞轮询的 goroutine 里回调，
// 用来打日志 / 更新界面提示；dbg 和 enabled 可以为 nil（nil 表示全部来源启用）。
func NewDialogFollow(onNavigate func(FollowEvent), dbg func(string, ...any), enabled func(kind string) bool) *DialogFollow {
	return &DialogFollow{seen: map[uintptr]int{}, onNavigate: onNavigate, dbg: dbg, enabled: enabled}
}

func (f *DialogFollow) logf(format string, args ...any) {
	if f.dbg != nil {
		f.dbg(format, args...)
	}
}

// LastDir 是最近一次记住的资源管理器目录（空表示还没见过）。
func (f *DialogFollow) LastDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDir
}

// Start 起轮询。用轮询而不是 SetWinEventHook：前台窗口变化 300ms 内就能看到，
// 而钩子回调里还要跨进程读地址栏，卡一下就把整个 shell 的 WinEvent 线程拖住了。
func (f *DialogFollow) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(explorerPollTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.poll()
			}
		}
	}()
}

func (f *DialogFollow) poll() {
	raw, _, _ := procGetForegroundWindow.Call()
	if raw == 0 {
		return
	}
	root := RootOwner(raw)

	f.mu.Lock()
	same := raw == f.lastFg
	if !same {
		f.lastFg = raw
	}
	src := f.lastSource
	f.mu.Unlock()

	if same {
		// 前台没变一般不用再往下走：读地址栏要往别人进程发消息，能不读就不读。
		// 但 tessoa 是个例外，见 pollActiveSource。
		f.pollActiveSource(root, src)
		return
	}

	switch {
	case f.isPathSource(root):
		f.mu.Lock()
		f.lastActive = root
		f.lastSource = root
		// 只要目录来源窗口重新成为前台，就算"用户刚去看过它"，版本号 +1，
		// 哪怕它的目录没变。否则"打开框后切到 Arke（原地没动）再切回来"不会跟随，
		// 用户必须先在 Arke 里换一次目录才生效。目录随后真变了，rememberDir 还会再 +1，
		// 多一次只影响版本差值，不影响判定。
		f.version++
		f.mu.Unlock()
		// 这里顺手读一次只是为了让"停在文件管理器里不动"也能被记住，
		// 真正决定导航目标的还是 handleDialog 里那次现场重读。
		f.rememberDir(f.folderOf(root))
		if sourceKind(root) == SourceTessoa {
			f.noteTitle(root)
		}

	case IsFileDialog(root):
		f.mu.Lock()
		prev := f.lastActive
		src := f.lastSource
		f.lastActive = root
		f.mu.Unlock()

		prevWasSource := prev != 0 && src != 0 && prev == src && f.isPathSource(src)
		_, seenBefore := f.dialogSeen(root)

		// 什么时候现场重读来源目录：
		//   - 刚从文件管理器切过来：必须读（对话框要的正是"此刻"的位置）；
		//   - 这个对话框以前出现过：也要读。用户可能是 Alt+Tab 去了一趟 Arke /
		//     资源管理器，再 Alt+Tab 回来；中途那个应用主窗口（不是对话框）可能
		//     成为过前台，prev 就不再等于来源窗口。只认 prev 的话会漏掉这种切换，
		//     而版本号已经变了，说明用户在框外确实换过目录。
		if src != 0 && f.isPathSource(src) && (prevWasSource || seenBefore) {
			// 不吃缓存：tessoa 切窗格 / Arke 双栏换栏都不会改变前台窗口，
			// 轮询期间记下的那个目录很可能还是上一个窗格的。
			f.rememberDir(f.folderOfFresh(src))
		}
		f.handleDialog(root, prevWasSource)

	default:
		f.mu.Lock()
		f.lastActive = root
		f.mu.Unlock()
	}
}

// pollActiveSource 处理"前台窗口没变，但文件管理器内部已经动了"。
//
// tessoa 是 Rust + wgpu 自绘的单窗口应用：切窗格、切布局、在同一个窗格里进目录，
// GetForegroundWindow 一直都是同一个 hwnd —— 只按"前台变化"判断的话，
// 它的目录会永远停在第一次看到的值（实测：4 窗格布局下焦点挪到左下角，
// 跟随器完全不知道。切换布局时更是如此）。
//
// 它的标题里带着活动目录名（"1tools — normal — tessoa (管理员)"），
// 于是拿标题当变更信号：标题一动才起进程问完整路径，常态零开销。
func (f *DialogFollow) pollActiveSource(root, src uintptr) {
	if src == 0 || root != src || sourceKind(src) != SourceTessoa || !f.sourceEnabled(SourceTessoa) {
		return
	}
	f.mu.Lock()
	if time.Since(f.lastTitleAt) < tessoaTitleInterval {
		f.mu.Unlock()
		return
	}
	f.lastTitleAt = time.Now()
	last := f.lastTitle
	f.mu.Unlock()

	title := WindowTextTimeout(src)
	if title == last {
		return
	}
	f.mu.Lock()
	f.lastTitle = title
	f.mu.Unlock()
	f.rememberDir(f.folderOf(src))
}

// noteTitle 记下某个窗口的当前标题，供 pollActiveSource 比对。
func (f *DialogFollow) noteTitle(hwnd uintptr) {
	title := WindowTextTimeout(hwnd)
	f.mu.Lock()
	f.lastTitle = title
	f.lastTitleAt = time.Now()
	f.mu.Unlock()
}

// rememberDir 目录真变了才升版本号。
func (f *DialogFollow) rememberDir(dir string) {
	if dir == "" {
		return
	}
	f.mu.Lock()
	changed := !strings.EqualFold(dir, f.lastDir)
	if changed {
		f.lastDir = dir
		f.version++
	}
	v := f.version
	f.mu.Unlock()
	if changed {
		f.logf("跟随: 记住目录 %q（版本 %d）", dir, v)
	}
}

// dialogSeen 报这个对话框以前出现过没有，以及它上次同步时的版本号。
func (f *DialogFollow) dialogSeen(hwnd uintptr) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.seen[hwnd]
	return v, ok
}

// shouldFollow 是"这次对话框成为前台，要不要送目录"的唯一判断。抽出来单测，
// 因为真实的窗口切换没法在 CI 里稳定复现（尤其 Alt+Tab 会夹一个应用主窗口）。
//
//   - 第一次见：只有刚从文件管理器切过来才送。否则可能是用户从任何程序里
//     打开的一个框，它原本停在哪就停在哪，不该被一个几小时前的目录拽走。
//   - 见过：只要期间版本号变过（用户在框外切到过文件管理器，换没换目录都算）就送，
//     **不管紧邻的上一个前台是不是文件管理器**。
//     这一条是为了 Alt+Tab：框开着 → Alt+Tab 去 Arke 换目录 → Alt+Tab 回来时
//     Windows 可能先把应用主窗（记事本本身）切成前台，用户再点一下框；这时
//     prev 是记事本而不是 Arke，只认 prev 就会漏。版本号变过就足以说明
//     用户去看过文件管理器，和中间夹了谁都无关。
//     只看目录变化不够：切到 Arke 但没动目录时版本不会升，用户会觉得"白切了"。
func shouldFollow(seenVersion int, seenBefore bool, version int, prevWasSource bool) bool {
	if !seenBefore {
		return prevWasSource
	}
	return seenVersion != version
}

// handleDialog 是 tracker 的核心规则：
//   - 框第一次出现、且上一次活动的是文件管理器 → 直接把 lastDir 送过去；
//     之前是"第一次只登记、等版本再变一次才送"，结果是打开框基本不跟随
//     （版本在框出现那一刻就已经是最新的，等它再变一次纯属碰运气）；
//   - 之后它再出现、而版本号变过（期间切到过文件管理器，换没换目录都算）→ 再送一次；
//   - 版本没变就一动不动，免得把用户手动在框里选的目录拽回去。
func (f *DialogFollow) handleDialog(hwnd uintptr, prevWasSource bool) {
	f.mu.Lock()
	seen, ok := f.seen[hwnd]
	version, dir := f.version, f.lastDir
	// 先登记版本再决定跳不跳：这次没跳（比如第一次出现时上一个前台不是
	// 文件管理器）也要记下基线，否则用户之后去文件管理器换了目录再回来，
	// 这个框会被永远当成"第一次"，还是不会跳。
	f.seen[hwnd] = version
	f.mu.Unlock()

	if dir == "" {
		return
	}
	if !shouldFollow(seen, ok, version, prevWasSource) {
		if !ok {
			f.logf("跟随: 对话框 %#x 第一次出现（版本 %d），上一个前台不是目录来源，不跳", hwnd, version)
		}
		return
	}
	if ok {
		f.logf("跟随: 对话框 %#x 版本 %d→%d，送到 %q", hwnd, seen, version, dir)
	} else {
		f.logf("跟随: 对话框 %#x 第一次出现（版本 %d），送到 %q", hwnd, version, dir)
	}

	// 跨进程写 + 回车，慢且可能被外壳拖着，扔到后台去，别卡住轮询。
	go func() {
		if NavigateDialog(hwnd, dir) && f.onNavigate != nil {
			f.onNavigate(FollowEvent{HWND: hwnd, Dir: dir})
		}
	}()
}
