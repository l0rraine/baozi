// baozi-hook 是独立的低级键盘钩子进程。
//
// 为什么必须是独立进程：
//  1. WH_KEYBOARD_LL 需要一个自己的消息循环，放进 WPF 式的 UI 主线程会互相拖累；
//  2. 钩子万一被反作弊、安全软件干掉，主程序还能活着（Lertaro 的 hook 进程同理）；
//  3. 主程序退出时通过关闭 stdin 通知本进程退出，不留孤儿。
//
// 它只做两件事：识别"双击 Ctrl"，以及在需要时抑制全屏应用中的唤出，
// 然后把一行事件写到 stdout，由主程序读取。
package main

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"time"
	"unsafe"

	"baozi/internal/config"
	"baozi/internal/win"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procSetWindowsHookExW  = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx     = user32.NewProc("CallNextHookEx")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
)

const (
	whKeyboardLL = 13

	wmKeyDown    = 0x0100
	wmKeyUp      = 0x0101
	wmSysKeyDown = 0x0104
	wmSysKeyUp   = 0x0105

	vkShift    = 0x10
	vkControl  = 0x11
	vkMenu     = 0x12
	vkLWin     = 0x5B
	vkRWin     = 0x5C
	vkLControl = 0xA2
	vkRControl = 0xA3
)

// KBDLLHOOKSTRUCT 与 Win32 定义一致。
type KBDLLHOOKSTRUCT struct {
	VkCode    uint32
	ScanCode  uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// MSG 与 Win32 定义一致（x64 下 48 字节）。
type MSG struct {
	HWND     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       struct{ X, Y int32 }
	LPrivate uint32
}

var (
	interval          = config.DefaultDoubleCtrlIntervalMs * time.Millisecond
	disableFullscreen = true

	lastCtrlUp time.Time
	armed      bool

	outMu sync.Mutex
	out   *bufio.Writer
)

func emit(event string) {
	outMu.Lock()
	defer outMu.Unlock()
	fmt.Fprintln(out, event)
	_ = out.Flush()
}

func isModifier(vk uint32) bool {
	switch vk {
	case vkShift, vkControl, vkMenu, vkLWin, vkRWin, vkLControl, vkRControl:
		return true
	}
	return false
}

func isCtrl(vk uint32) bool {
	return vk == vkControl || vk == vkLControl || vk == vkRControl
}

// onKey 是双击 Ctrl 的状态机。
//
//	Ctrl↓ Ctrl↑            → armed（等待第二次）
//	Ctrl↓（200ms 内）       → 触发
//	中间按过任何非修饰键     → 作废，避免 Ctrl+C 被当成唤出
func onKey(msg uint32, vk uint32) {
	down := msg == wmKeyDown || msg == wmSysKeyDown
	if !isCtrl(vk) {
		if down && !isModifier(vk) {
			armed = false
		}
		return
	}
	if !down {
		lastCtrlUp = time.Now()
		armed = true
		return
	}
	if armed && time.Since(lastCtrlUp) <= interval {
		armed = false
		fire()
	}
}

func fire() {
	if disableFullscreen {
		fg := win.ForegroundInfo()
		if fg.Fullscreen {
			fmt.Fprintf(os.Stderr, "suppressed in fullscreen (class=%q)\n", fg.Class)
			return
		}
	}
	emit("toggle")
}

var hookCallback = windows.NewCallback(func(nCode int, wParam, lParam uintptr) uintptr {
	if nCode >= 0 {
		onKey(uint32(wParam), kbStruct(lParam).VkCode)
	}
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return ret
})

// kbStruct 把 lParam 当作指针读出来。
// 不直接写 unsafe.Pointer(lParam) 是为了避开 vet 的 unsafeptr 误报——
// 这里的 lParam 本来就是系统传进来的 KBDLLHOOKSTRUCT 指针。
func kbStruct(lParam uintptr) *KBDLLHOOKSTRUCT {
	return *(**KBDLLHOOKSTRUCT)(unsafe.Pointer(&lParam))
}

func main() {
	cfg, _, err := config.Load()
	if err != nil || cfg == nil {
		fmt.Fprintf(os.Stderr, "config: %v (using defaults)\n", err)
		cfg = config.Default()
	}
	if cfg.Hotkey.DoubleCtrlIntervalMs > 0 {
		interval = time.Duration(cfg.Hotkey.DoubleCtrlIntervalMs) * time.Millisecond
	}
	disableFullscreen = cfg.Hotkey.DisableInFullscreen
	if !cfg.Hotkey.DoubleCtrl {
		// 用户关了双击 Ctrl：进程仍然起来，只是不触发，便于之后热启用。
		interval = -1
	}

	out = bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// 主程序退出会关闭 stdin；读到 EOF 就退出，避免孤儿进程。
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			if _, err := reader.ReadString('\n'); err != nil {
				emit("exit")
				os.Exit(0)
			}
		}
	}()

	hook, _, callErr := procSetWindowsHookExW.Call(whKeyboardLL, hookCallback, 0, 0)
	if hook == 0 {
		fmt.Fprintf(os.Stderr, "SetWindowsHookEx failed: %v\n", callErr)
		emit("error")
		os.Exit(1)
	}
	emit("ready")
	fmt.Fprintf(os.Stderr, "hook installed (tid=%d, interval=%s, suppressFullscreen=%v)\n",
		currentThreadID(), interval, disableFullscreen)

	var msg MSG
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		switch int32(ret) {
		case -1: // 错误
			return
		case 0: // WM_QUIT
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func currentThreadID() uint32 {
	id, _, _ := procGetCurrentThreadID.Call()
	return uint32(id)
}
