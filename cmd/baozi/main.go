// Baozi 是一个 Windows 快速启动器：双击 Ctrl 唤出，搜索文件/应用，
// 按「频次 > 类型 > 准确性」排序，空查询时展示使用历史。
//
// 架构对应关系（与 Lertaro 的差异写在 docs/architecture.md）：
//   - 本进程 = 用户态交互层（对应 Lertaro.App）
//   - baozi-hook = 键盘钩子与窗口适配层（对应 Lertaro.Service --hook）
//   - 索引：当前在进程内后台重建；接入 USN/MFT 后改为独立 SYSTEM 服务（对应 Lertaro.Service）
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof" // BAOZI_PPROF=1 时才真的监听，平时只是注册 handler
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"baozi/internal/config"
	"baozi/internal/history"
	"baozi/internal/index"
	"baozi/internal/pinyin"
	"baozi/internal/search"
	"baozi/internal/update"
	"baozi/internal/win"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

const (
	// 面板宽高的默认值。实际尺寸在 config.PanelSize() 里（设置页可改，0 = 用这两个），
	// 保留成常量是因为视图测试和预览需要一块确定的画布。
	panelWidth  = config.DefaultPanelWidth
	panelHeight = config.DefaultPanelHeight

	// 结果行左侧图标的尺寸不再写死：默认跟着系统缩放走，也能在设置里指定，
	// 见 config.BaseIconSize / Config.IconSizeAt 与 application.iconSize。
	// 之前是固定的 22 DIP，在 125% 缩放的屏上（这台机器就是）显得偏小。

	// cardWidthMin 是首页卡片（也就是悬停/选中时那块圆角底）的最小宽度，cardGap 是列间距，
	// cardGutter 是网格左右留白。一排几张不写死：列数按这三个数算（application.gridCols）。
	// 实际宽度由 a.cellWidth() 给：图标调大时卡片跟着变宽，名字才不会被挤成两行。
	//
	// 这组数照用户给的启动器截图（470×119 那一排五个）量的比例定：图标之间的空隙
	// 约等于图标宽的 0.6 倍，而底色块只比图标大一圈。原来卡片 84 宽、间距 6，块比图标
	// 宽出一大截、图标之间反而贴得近，看着"块是块、图标是图标"两块互不相干。
	cardWidthMin = 86
	cardGap      = 12
	// cardRowGap 是行间距，比列间距小得多：参考图里下一排图标紧跟着上一排的名字。
	// 之前行、列共用 cardGap，第二排被推得太远，整片网格显得散。
	cardRowGap = 12
	cardGutter = 18
	// panelCols 是默认宽度（720）下一排的卡片数，只给测试与默认布局当基准用；
	// 真正渲染时一律走 a.gridCols()。
	panelCols = (panelWidth - 2*cardGutter + cardGap) / (cardWidthMin + cardGap)

	// maxPinned 是首页「已固定」区最多显示几个。
	maxPinned = 12
)

var searchIcon = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>`))

// debugKeys 打开后在 stderr 打印每次查询，排查"输入到底有没有到面板"。
var debugKeys = os.Getenv("BAOZI_DEBUG") != ""

// appEntry 是开始菜单应用 + 预先算好的拼音串（应用只有几百个，值得每次启动算一遍）。
type appEntry struct {
	mygo.InstalledApp
	initials string // "豆包工作" → "dbgz"，空串表示名字是纯 ASCII
	full     string // "豆包工作" → "doubaogongzuo"
	// alias 是从 ID 抽出来的产品名（Microsoft.WindowsCalculator → "Microsoft WindowsCalculator"）。
	// 它代替 Path 参与匹配，理由见 search.AppAlias。
	alias string
}

type application struct {
	cfg     *config.Config
	cfgPath string
	dataDir string

	panel *mygo.Window
	tray  *mygo.Tray
	// menu 是托盘菜单：设置里改了自启后要回头同步勾选状态，不然两处会打架。
	menu *mygo.Menu

	hist *history.Store

	// cat 是当前索引快照。快照自己带着名字 arena 一起换，
	// 旧快照在被替换后依然有效（搜索进行中的那次读的是旧表，不会看到半新半旧的数据）。
	cat  atomic.Pointer[index.Table]
	apps atomic.Pointer[[]appEntry]
	// appsMTime 是上一次读应用列表时开始菜单目录的最新 mtime（UnixNano），
	// appsReloading 保证同一时间只有一次重读；appsChecked 是上次检查它的时间
	// （主线程），避免每次唤出都递归走一遍开始菜单。
	appsMTime     atomic.Int64
	appsReloading atomic.Bool
	appsChecked   time.Time

	// 增量同步状态（只由 refreshIndex 读写，用 idxMu 串行化）。
	// idxPath 是已载入的那份快照文件的完整路径（可能是 index.bin，也可能是
	// index-<时间戳>.bin 这种"新一代"，见 index.Save）；idxBase 是它的代次标识；
	// idxSeq 是已经叠到服务写出的第几批；idxOps 是这一代里已经叠了多少条指令
	// —— 服务每次都整份重写 delta 的 ops，所以进度必须按"条"记，不能按"批"。
	idxMu    sync.Mutex
	idxPath  string
	idxMtime time.Time
	idxBase  uint64
	idxSeq   uint64
	idxOps   int

	query   string
	results []search.Result
	chosen  int
	// pinnedCount 是 results 末尾属于「已固定」的条数（首页网格分段用，
	// 前 len(results)-pinnedCount 条是最近使用）。
	pinnedCount int
	list        ui.ListState
	input       ui.Handle
	icons       map[string]*ui.Bitmap
	// frost 是整屏的磨砂图：唤出前抓一次全屏、模糊、压白做出来的（见 glass.go）。
	// 面板里铺的就是这一张，尺寸等于整个虚拟屏，位置给负值 —— 窗口等于在图上取景，
	// 拖到哪就露出哪块桌面，所以不用每帧重新裁图、重新传纹理。
	frost  *ui.Bitmap
	frostW float32
	frostH float32
	// glassOffX/Y 是窗口在整屏磨砂图上的取景偏移（DIP，负数），
	// 面板一移动就重算，通常由 glassOffset() 给出。
	glassOffX float32
	glassOffY float32
	// frostPixW/H 是磨砂图的像素尺寸，用来判断能不能复用同一张纹理。
	frostPixW int
	frostPixH int
	// frostSmall 是最近一次做出来的磨砂小图（整屏缩略、模糊、压白之后）。
	// 留着它只是为了在阴影层里取"玻璃本体颜色"：弧那一圈要用玻璃色和桌面色
	// 做面积混合，两侧颜色都必须和邻居同源。
	frostSmall *image.RGBA
	// frostBase 是唤出时抓的那张"未模糊、未压白"的桌面底片。设置页改完透明度/
	// 模糊要当场重做磨砂图，就得有这张干净的原件（Blur/Tint 都是原地改图）。
	// 面板收起时随 frostSmall 一起放掉，省 230KB。
	frostBase *image.RGBA
	// frostGen 是磨砂图的代次：每重做一次（改透明度/模糊、拖动糊化、重新抓屏）就 +1。
	//
	// 它必须参与底图的形状键（见 pane.go 的 keyAt）：底图里每个玻璃像素都是拿磨砂图
	// 采样来的，磨砂图一换，旧底图整张作废。早先键里没有它，于是"设置页把透明度
	// 从半透明改成不透明、点保存"之后，屏幕上一点变化都没有 —— 磨砂图确实重做了，
	// 但三块玻璃都认为形状没变、谁都不重烘。
	frostGen int
	// screenW/H/X/Y 是整屏快照的尺寸与屏幕原点（物理像素），算取景偏移时要用。
	screenW int
	screenH int
	screenX int
	screenY int
	// deskColor 是面板外面那块桌面的颜色。圆角裁掉的四个角由它兜底 ——
	// 窗口是 noRedirect（DirectComposition）合成的，透明的地方透不出桌面，
	// 只会露出一块白（#FFFFFF），跟桌面的 #FCFCFC 差一点点，看上去就是"四个白角"。
	deskColor ui.Color
	deskSet   bool
	// deskCorners 是窗口**四角各自**的桌面颜色（左上、右上、左下、右下）。
	//
	// Win10 的窗口区域是 1 位掩码，`CreateRoundRectRgn` 天生没有抗锯齿：四角那条
	// 弧永远是阶梯。既然边缘本身柔不了，就让**边界两侧的颜色尽量一样** —— 月牙
	// 里铺该角附近真实的桌面像素色，台阶就落在同一个颜色里，看不见。
	// 用一个平均色垫四个角不够：壁纸色调一不均，角上就显出一块和旁边不同的色，
	// 那条阶梯立刻露出来。
	deskCorners    [4]ui.Color
	deskCornersSet bool
	// cornerPatches 是四角那一小块**真实桌面**的逐像素副本，在窗口显示之前抓。
	// 月牙弧外就照抄它，于是弧外和真桌面 1:1 同色 —— 只用一个"角上取色"时，
	// 壁纸在那里有渐变（实测弧外铺 #79C5F7、紧挨着的真桌面是 #15A4F2），
	// 月牙就成了壁纸上一道亮带。取不到（尺寸对不上、抓图失败）就退回 deskCorners。
	cornerPatches [4]*image.RGBA
	// deskHalf 是唤出那一刻的**半分辨率整屏快照**，专门给四角"弧外"那块当底色用。
	// 为什么不用 BitBlt 现场抓的 patch：面板一被拖动/缩放，那块 patch 就过期了
	// （抓的是上一处位置），壁纸是照片时颜色明显串味，屏幕上就是"角上画了一道弧"。
	// 半分辨率（2560×1440 → 1280×720，3.7 MB）够用：那块本身就在阴影下面，糊一点看不出来。
	deskHalf                           *image.RGBA
	deskHalfX, deskHalfY, deskHalfStep int
	// pane 是面板这块玻璃的共享状态：底图（整窗自绘那张，桌面 + 玻璃 + 边框按
	// 覆盖率相加，四角的抗锯齿就是它给的）、合成缓冲、形状键、取景原点、投影层。
	//
	// 它和设置窗、文件互传窗用的是**同一个类型**（pane.go 的 paneState）——
	// 以前这三块各有一份状态、各写一套刷新，抽的时候漏一处就出一次观感问题。
	// 现在底图与投影的算法只有一份，面板独有的东西（四角月牙、边框弧色、
	// 整屏磨砂图、区域、动画）留在 application 上。
	pane paneState
	// screenGen 每次重新抓屏 +1：底图依赖快照内容，快照一换就必须重合成。
	screenGen int
	// cornerBitmaps 是四角那四张"月牙"位图，cornerPx 是它们的边长（物理像素）。
	// 弧外铺 deskCorners 的桌面色，弧上按覆盖率渐隐到透明 —— 圆角那条内边界
	// 的抗锯齿就是它们给的。变了桌面色或换了面板尺寸就重建（见 buildCornerBitmaps）。
	cornerBitmaps [4]*ui.Bitmap
	cornerPx      int
	// cornerEdge 是圆角边框弧的颜色（0xRRGGBB），传给自绘阴影层 —— 弧那一段
	// 的边框现在由它画：面板的画布被 SetWindowRgn 裁掉了，画不出去。
	cornerEdge uint32
	// cornerSize 是上面四张图当前**实际的像素边长**。它和 cornerPx（沿用的大
	// 尺寸）分开记：尺寸没变时只能 Update 像素、不能换新 Bitmap —— ui.Image
	// 渲染一次就把纹理缓存住了，换对象屏上不会变。
	cornerSize   int
	cornerKeySet bool
	cornerKeyVal cornerKey
	// maskKey / maskCache 缓存四角月牙的覆盖率掩码（每个角 size² 个 float64，
	// 40×40 一共 51 KB）。掩码只跟圆角半径和位图边长有关，缩放时反复重建的是
	// 桌面色，掩码根本不用重算 —— 每次重算要 40 万次 hypot 加八遍 3×3 柔化。
	maskKey   cornerMaskKey
	maskCache [4][]float64
	// cornerTimer 把缩放过程中连珠炮般的月牙重建攒成一次：WM_SIZE 一帧来一次，
	// 每帧都重画四张图（外加四张 GPU 纹理）就是"缩放卡卡的"另一半原因。
	// 月牙的边长只跟圆角半径有关，和面板多大无关，所以缩放期间沿用旧图完全正确。
	cornerTimer *time.Timer
	// regionQueryLogged 保证"设完区域却查不到"这条诊断只记一次（它一旦成立就是
	// 每帧都成立，重复记只会刷日志）。
	regionQueryLogged bool
	// panelUp 是"面板此刻应该是开着的"这个事实（show 里置 true，hidePanel 里置 false）。
	// 为什么不用 panel.IsVisible()：面板是 noRedirect 的合成窗口，系统的
	// IsWindowVisible 在它身上会撒谎（实测报 false），而定期复查外观（ensureShell）
	// 必须有个可靠的开关，否则一次都不跑。
	panelUp atomic.Bool
	// shellProbes 数"每次唤出之后已经记过几次外观复查"，用来给诊断行限流。
	shellProbes int
	// resizeFrames/resizeSum/resizeMax/resizeGapMax 只用来回答一个问题：用户拖得
	// 很快的时候，到底是我们的回调慢，还是 Windows 把事件并掉了。停手时
	// finishResize 打一行"缩放: 本轮 N 帧 …"，N 远小于鼠标移动次数就说明
	// 是事件被并了（面板自己的绘制跟不上），而不是我们的回调。
	resizeFrames    int
	resizeSum       time.Duration
	resizeMax       time.Duration
	resizeGapMax    time.Duration
	resizePrev      time.Time
	resizeStartedAt time.Time
	// lastMoveAt / moveTimer 给拖动（移动）那条路径节流用：见 OnMove。
	lastMoveAt       time.Time
	moveTimer        *time.Timer
	moveUpdateQueued bool
	// uiTick / dragUntil / hangDumped 是看门狗用的（见 startWatchdog）：拖动期间
	// UI 线程若真有几百毫秒不响应，就把所有 goroutine 的栈打进日志 —— 光看回调
	// 耗时抓不到死锁，栈才说得清它卡在哪一行。
	uiTick     atomic.Int64
	dragUntil  atomic.Int64
	hangDumped atomic.Bool
	// dragFade 是"拖动中把面板变成不透明白"的开关（见 glass.go 的 setDragFade
	// 与 backing.go 的 dragWhiteBitmap）。面板挪动和内容提交分属相邻两帧，
	// 那一帧的相对位移在快拖时有十几像素；面板里没有桌面细节可比就不见了。
	dragFade bool
	// dragWhite 是拖动时盖上去的那张白面板（缓存的纹理，尺寸/半径变了才重建）。
	dragWhite              *ui.Bitmap
	dragWhiteW, dragWhiteH int
	dragWhiteR             float64
	dragWhiteDark          bool // 这张白面板是按亮/暗桌面生成的
	// redrawNext 是"拖动时自己推一帧上屏"的节流点（见 OnMove 里的 win.RedrawNow）。
	// 拖动走系统模态循环，帧请求在那儿排队，光算完底图屏幕不会变。
	redrawNext atomic.Int64
	// selfDrag 是面板的自绘拖动（见 dragwin.go）。系统拖动先挪窗口、我们追内容，
	// 快拖时差半帧看得出来；自己接管以后"先算内容、再挪窗口、再提交"，同一帧。
	selfDrag *selfDrag
	// dragRetryLeft 是"还没摸到 MyGoSurface 时还能再试几拍"的余量：mygo 的子窗口
	// 是 Show 之后才建出来的，偶尔差一拍。每成功挂上一次就补满。
	dragRetryLeft int
	// deskDark 记录这次唤出时那张桌面底片是不是暗的：暗就切深色配色、玻璃压深，
	// 亮就切浅色配色、玻璃压白。由 glass.go 的 isDarkImage 在抓完快照时判定。
	deskDark bool
	// shadow 是垫在面板底下的自绘阴影层（一个 layered 窗口）。DWM 在 Win10 上
	// 不给"设了窗口区域"的窗口画阴影，而圆角只能靠区域，所以自己画（见
	// internal/win/shadow.go）。投影层随玻璃状态走，已并入 a.pane.shadow。
	// borderColor 是最后一帧用的面板边线色，四角月牙要把这条边一起画进去
	// （mygo 画边框的圆角段没有抗锯齿，那正是四角台阶的第二个来源）。
	borderColor ui.Color
	borderSet   bool
	// thumbAsk 记录"已经去要过缩略图的键"（失败的也算），避免每帧重复下单。
	// thumbSem 限制同时在取的缩略图个数：Shell 那边是 COM 走 explorer 的缩略图
	// 提供者，一次放几十个进去只会互相拖慢，4 个足够铺满一屏。
	thumbAsk map[string]bool
	thumbSem chan struct{}
	// offline 为 true 时不向 Shell 要图标（视图测试里没有消息循环，要了会永久阻塞）。
	offline bool
	// scale 是显示器的最大缩放比例，启动时问一次就缓存：Screen.Displays 也走
	// onMainValue，视图测试里现问会永久阻塞。0 表示还没问过，按 1 处理。
	scale float32

	// 首页四向键的按住状态：焦点在搜索框时 Shortcut 收不到左右键（被输入框吃掉），
	// 只能读按住状态做边沿检测（keyStep）。
	heldLeft, heldRight, heldUp, heldDown bool

	// follow 是文件对话框跟随：记住文件管理器最近待过的目录，那个「打开 / 另存为」框
	// 再次回到前台时把路径送过去（Lertaro 的 IActivePathCollector + NavigationTracker）。
	follow       *win.DialogFollow
	followCancel context.CancelFunc
	// sources 是"现在哪些目录来源开着"。跟随器在自己的 goroutine 里每 200ms 问一次，
	// 而配置指针在保存时会被整体替换，所以这里用原子指针存一份快照，避免数据竞争。
	sources atomic.Pointer[map[string]bool]

	status string // 索引/钩子状态，显示在底栏

	// regexOn 是搜索框右边那个 ".*" 按钮的状态：开着时查询按正则解释。
	// 正则模式下只做精确匹配 —— 不走拼音、不走模糊子序列、不做准确度分档。
	// 不落盘：这是这次打开面板期间的一个模式，重启回到普通搜索。
	regexOn bool
	// patternErr 是上一次"模式型查询"（正则或通配符）编译失败的原因，
	// 显示在结果区里（空表示没问题）。两种模式共用一格：同一时刻只会走其中一条。
	patternErr string

	// quitting 为 true 表示正在真正退出：窗口的 OnClose 不再拦关闭。
	// mygo 的 Quit 是逐个关窗口，任何窗口 PreventDefault 都会让整个 Quit 作废。
	quitting atomic.Bool

	hook     *exec.Cmd
	hookIn   io.WriteCloser
	quitOnce sync.Once
	scanMu   sync.Mutex
	scanOff  context.CancelFunc
	// scanWG 跟踪后台扫描。卸载 / 退出时等它收尾，否则 RemoveAll 会和索引落盘打架
	// （表现就是数据目录"删了又回来"）。
	scanWG sync.WaitGroup

	settings *settingsWindow
	share    *shareWindow

	// uninstalling 为 true 表示正在卸载：收尾时不再落盘历史（数据目录马上要整个删掉）。
	uninstalling atomic.Bool

	// openAtLogin 是开机自启的当前状态。它在 ready() 里问系统一次就缓存住：
	// mygo 的 App.OpenAtLogin 走 onMainValue，必须在 native 主循环里调用，
	// 而设置窗口的视图在 ui.Tester 下渲染（没有主循环），每次 load 去问会永久阻塞。
	openAtLogin atomic.Bool

	// 更新检查的结果（"关于"页显示它）。检查本身跑在后台 goroutine 里，
	// 任何时刻都不许碰 UI 线程 —— 见 updatecheck.go。
	upd updateState

	// 右键菜单里选的动作不能在菜单回调里直接改 a.results：
	// 那一帧正在渲染这条列表，边遍历边重建会越界。先记下来，视图开头统一处理。
	pendingOp  string // "" | "delete" | "pin" | "unpin"
	pendingIdx int
	// 首页「已固定」区里拖动排序：同样不能在卡片渲染途中改 results，
	// 松手那一帧记下起点与落点，下一帧 view 开头结算。
	pendingPinMove *pinMove

	// pinDragPts 存每张固定卡片本帧的拖动偏移。Draggable 要求这个指针跨帧稳定，
	// 每帧新建一个 Point 会把拖动中的卡片拽回原位。
	pinDragPts    map[int]*ui.Point
	pinDragFrom   int
	pinDragActive bool
	pinDragX      float32
	pinDragY      float32
	// pinHandles/pinRects 记录本帧「已固定」卡片上一帧提交的矩形。自绘窗口拖动在
	// MyGoSurface 上子类化，比 UI 先拿到鼠标，要按屏幕坐标反查固定卡，
	// 避免把卡片拖动抢成窗口拖动。Element 跨帧会失效，所以用 Handle 读矩形。
	pinHandles map[int]*ui.Handle
	pinRects   []ui.Rect

	// listBar 是结果列表那条竖滚动条的**可按下区域**（上一帧提交的矩形）。
	// 和固定卡是同一个道理：窗口拖动在 MyGoSurface 上子类化，比 UI 先拿到鼠标，
	// 按在滚动条上拖时会被它当成"拖面板"，滚动条一步都动不了。按下那一点先问
	// 这里，命中就让开，把按下原样交给 mygo 的 scrollbarPress。
	//
	// 只有一条：主面板里会滚的只有一个结果列表。设置/互传子窗的拖动只接管顶栏，
	// 内容区的滚动条本来就不冲突。
	listBar ui.Rect

	// hoverPoint 是上一帧指针在窗口里的位置。悬停选中只在它**变了**的时候接管，
	// 否则鼠标停在某张卡上不动、用户按方向键，选中会被 hover 每帧拽回去。
	hoverPoint ui.Point
	// hoverKnown 标记 hoverPoint 有没有值：首帧（0,0）不能当成"指针没动"，
	// 否则面板一唤出、指针正好在左上角时就永远不接管。
	hoverKnown bool

	// 面板改尺寸、拖着面板走：这两个回调在拖动过程中会连着来几十次，
	// 攒到用户停手才写盘，否则每动一像素就重写一遍 config.json。
	// 尺寸和位置共用同一个定时器，免得两个 goroutine 各写一份、互相覆盖。
	panelMu    sync.Mutex
	panelPendW int
	panelPendH int
	panelPendX int
	panelPendY int
	panelSave  *time.Timer
	// panelPlaceAt 是上一次"刚把面板摆到目标位置"的时刻。面板弹出的头几百毫秒里
	// mygo / 系统还会把它挪几下（实测过，能把配好的位置一路滑走），那些移动不是
	// 用户拖的，不能记进配置，否则"记住位置"就变成"记住每次被挪到哪"。
	panelPlaceAt time.Time
}

// pinMove 是一次拖动排序的意图，from/to 都是 results 的下标。
type pinMove struct {
	from, to int
}

// panelFont 是面板和设置窗的全局字体。
//
// Windows 上"系统 UI 字体"其实是两族拼起来的：拉丁字母走 Segoe UI、汉字回退到
// Microsoft YaHei UI。同一个界面里两种字形混排，字宽和字重对不上，看着就"不齐"——
// 面板里到处是"中文 + xxx.exe"这种混排，落差尤其明显。明写雅黑 UI（它自带完整
// 拉丁字形）让中英同族，字号也才真的对得上。
//
// 不写 "Segoe UI Variable Text"：那是 Windows 11 才有的字体族，这台机器（Win10 22H2）
// 没有，写进去只会静默回退成别的。
const panelFont = "Microsoft YaHei UI"

// panelBorder 是面板要不要那一圈 1px 边线。
//
// 现在关掉：整圈边线（直边那八条细线 + 自绘层那条环）都不画，面板只剩玻璃到
// 壁纸的柔和过渡。要恢复边框把它改回 true，并把 glass.go 里的 BorderW 一起
// 改回去。
const panelBorder = false

// fontOn 让视图测试能跳过设字体。mygo 解析字体族要回 native 主线程，
// 而 ui.Tester 下没有主循环，设了会永久阻塞——和 Shell.* / mygo.App.* 在测试里
// 卡死的毛病一样。测试环境本来也不需要看字形。
var fontOn = true

// usePanelFont 把当前主题的默认字体换成面板那一套。
func usePanelFont(t *ui.Theme) {
	if fontOn {
		t.Font = panelFont
	}
}

// resultKey 是结果在历史里的键：普通项用路径，命令用关键字。
func resultKey(r search.Result) string {
	if r.Path != "" {
		return r.Path
	}
	return r.Command
}

// applyPending 执行上一帧右键菜单里选的动作、以及拖动排序的结果。
// 只在 view 开头调用（那时还没开始遍历结果）。
func (a *application) applyPending() {
	if mv := a.pendingPinMove; mv != nil {
		a.pendingPinMove = nil
		a.applyPinMove(*mv)
	}
	if a.pendingOp == "" {
		return
	}
	op, idx := a.pendingOp, a.pendingIdx
	a.pendingOp = ""
	if idx < 0 || idx >= len(a.results) {
		return
	}
	r := a.results[idx]
	key := resultKey(r)
	switch op {
	case "folder":
		a.openFolderAt(idx)
	case "delete":
		if a.hist.Remove(key) {
			a.status = "已从历史删除：" + r.Name
		} else {
			a.status = "这条不在历史里：" + r.Name
		}
	case "unpin":
		if a.hist.Unpin(key) {
			a.status = "已取消固定：" + r.Name
		}
	case "pin":
		if a.hist.Count(key) == 0 {
			e := entryOf(r)
			if e.Path == "" {
				e.Path = key
			}
			a.hist.TouchEntry(e)
		}
		if pinned, ok := a.hist.TogglePin(key); ok {
			if pinned {
				a.status = "已固定到首页：" + r.Name
			} else {
				a.status = "已取消固定：" + r.Name
			}
		}
	}
	// 首页的两段（最近 / 已固定）都由历史推出来，删/固定之后必须重建。
	if a.query == "" {
		a.results = a.historyResults(12)
		a.choose(0)
		return
	}
	// 搜索态下结果来自索引：删的只是历史记录，命中项下次空查询才消失。
	if op == "delete" {
		a.status = "已从历史删除（搜索仍能搜到）：" + r.Name
	}
}

// applyPinMove 把一次拖动排序落进历史，然后重建首页。
func (a *application) applyPinMove(mv pinMove) {
	recentTo := len(a.results) - a.pinnedCount
	if a.pinnedCount < 2 || mv.from < recentTo || mv.to < recentTo ||
		mv.from >= len(a.results) || mv.to >= len(a.results) {
		return
	}
	keys := make([]string, 0, a.pinnedCount)
	for i := recentTo; i < len(a.results); i++ {
		keys = append(keys, resultKey(a.results[i]))
	}
	relFrom, relTo := mv.from-recentTo, mv.to-recentTo
	k := keys[relFrom]
	keys = append(keys[:relFrom], keys[relFrom+1:]...)
	keys = slices.Insert(keys, relTo, k)
	if !a.hist.ReorderPinned(keys) {
		return
	}
	a.status = fmt.Sprintf("已保存固定顺序：%s 移到第 %d 位", a.results[mv.from].Name, relTo+1)
	a.results = a.historyResults(maxPinned)
	a.choose(recentTo + relTo)
}

// pinDragPoint 取某张固定卡片跨帧复用的拖动偏移指针。
func (a *application) pinDragPoint(i int) *ui.Point {
	if a.pinDragPts == nil {
		a.pinDragPts = map[int]*ui.Point{}
	}
	pt, ok := a.pinDragPts[i]
	if !ok {
		pt = &ui.Point{}
		a.pinDragPts[i] = pt
	}
	return pt
}

// pinPressHit 判断窗口客户区坐标是否落在某张固定卡片里。
//
// 自绘拖动挂在 MyGoSurface 的子类化上，比 mygo 的 UI 先看到鼠标移动：
// 只要它自己接管，固定卡的 Draggable 就再也收不到消息，表现就是拖卡片时
// 整块面板跟着走。所以按下的那一点要先问这里，命中固定卡就把窗口拖动让开。
func (a *application) pinPressHit(x, y float32) bool {
	for _, b := range a.pinRects {
		if b.W <= 0 || b.H <= 0 {
			continue
		}
		if x >= b.X && x < b.X+b.W && y >= b.Y && y < b.Y+b.H {
			return true
		}
	}
	return false
}

// scrollBarPressHit 判断窗口客户区坐标是否落在结果列表的滚动条上。
//
// 同一个毛病、同一个解法：滚动条的拖动是 mygo 自己在做（scrollbarPress 拿住
// 拇指、按指针位置换算偏移），但窗口拖动比它先收到 WM_MOUSEMOVE，
// 位移一过阈值就把这次按住改判成"拖面板" —— 用户看到的就是"想滚列表，
// 整个窗口跟着走了"。命中这条竖条就把 d.pending 清掉，按下和移动原样转发。
func (a *application) scrollBarPressHit(x, y float32) bool {
	b := a.listBar
	if b.W <= 0 || b.H <= 0 {
		return false
	}
	return x >= b.X && x < b.X+b.W && y >= b.Y && y < b.Y+b.H
}

// defaultScrollbarWidth 是 mygo 没设 Theme.ScrollbarWidth 时的默认拇指宽度（6 DIP），
// 取自 ui/theme.go 的 scrollbarWidth()。面板不显式设这一项，所以就是 6。
const defaultScrollbarWidth = 6

// listBarStrip 返回滚动容器 box 的**竖滚动条可按下区域**。
// mygo（paint.go 的 scrollBars）里竖条贴着容器右边缘，宽 ScrollbarWidth，
// 而可接按下的轨道比它再宽 6 DIP（左右各 3，给"按下时变粗"留位置），
// 所以这里取 width+6 一整条。box 为空（列表还没提交过一帧）就返回零矩形。
func listBarStrip(box ui.Rect, t *ui.Theme) ui.Rect {
	if box.W <= 0 || box.H <= 0 || t == nil {
		return ui.Rect{}
	}
	w := t.ScrollbarWidth
	if w <= 0 {
		w = defaultScrollbarWidth
	}
	track := w + 6
	if track > box.W {
		track = box.W
	}
	return ui.Rect{X: box.X + box.W - track, Y: box.Y, W: track, H: box.H}
}

// pinHandle 取某张固定卡片跨帧复用的 Handle：卡片每帧重建，
// 但 Handle 能读到上一帧提交后的矩形。
func (a *application) pinHandle(i int) *ui.Handle {
	if a.pinHandles == nil {
		a.pinHandles = map[int]*ui.Handle{}
	}
	h, ok := a.pinHandles[i]
	if !ok {
		h = &ui.Handle{}
		a.pinHandles[i] = h
	}
	return h
}

// trackPinDrag 把这张卡片的拖动状态接起来：拖动中记录最新偏移，
// 松手那一帧把偏移归零（否则卡片会粘在拖到的位置），并记下这次移动的意图。
func (a *application) trackPinDrag(i int, el ui.Element) {
	pt := a.pinDragPoint(i)
	if el.Pressed() {
		if !a.pinDragActive || a.pinDragFrom != i {
			a.pinDragActive, a.pinDragFrom = true, i
		}
		o := el.DragOffset()
		a.pinDragX, a.pinDragY = o.X, o.Y
		return
	}
	if !a.pinDragActive || a.pinDragFrom != i {
		return
	}
	a.pinDragActive = false
	*pt = ui.Point{}
	a.pendingPinMove = a.pinMoveFor(i, a.pinDragX, a.pinDragY)
	a.pinDragX, a.pinDragY = 0, 0
}

// pinMoveFor 把拖动位移换算成落点。
// 卡片 84 宽、行内间距 6，所以一列 90；行高随图标块的尺寸走（块 + 57，
// 其中 57 = 上下 padding 8 + 图标块与名字的间距 5 + 三行字 38 + 行间距 6），
// 图标块 32 时正好是 89。这两组数字要跟 card() 与 homeGrid() 保持一致。
func (a *application) pinMoveFor(from int, dx, dy float32) *pinMove {
	recentTo := len(a.results) - a.pinnedCount
	if a.pinnedCount < 2 || from < recentTo {
		return nil
	}
	// 设置里把图标调大了，卡片跟着变高，落点判定也得跟着换，否则拖一格会跳两格。
	// 行高 = 上下 padding 8 + 图标 + 图标与名字的间距 6 + 两行字 28.8 + 行间距 6。
	rowStep := a.cardIcon() + cardRowGap + 43
	colStep := a.cellWidth() + cardGap
	colShift := int(math.Round(float64(dx) / float64(colStep)))
	rowShift := int(math.Round(float64(dy) / float64(rowStep)))
	if colShift == 0 && rowShift == 0 {
		return nil
	}
	rel := from - recentTo
	to := rel + colShift + rowShift*a.gridCols()
	if to < 0 {
		to = 0
	}
	if to >= a.pinnedCount {
		to = a.pinnedCount - 1
	}
	if to == rel {
		return nil
	}
	a.pinDragPts = nil // 位置要变了，旧的偏移指针不能再对号入座
	return &pinMove{from: from, to: recentTo + to}
}

func (a *application) view(c *ui.Context) {
	// 玻璃上原来那套浅色字读不出来，所以玻璃模式下整块面板走浅色主题：
	// 深色字 + 浅底。桌面是深色的时候反过来 —— 浅色主题的深色字压在暗玻璃上
	// 几乎看不见（用户报的"暗色背景变成这样"）。判据是桌面底片的平均亮度，
	// 见 glass.go 的 isDarkImage：跟壁纸走，不跟系统的明暗外观走。
	if a.frost != nil {
		th := ui.LightTheme()
		if a.deskDark {
			th = ui.DarkTheme()
		}
		// 圆角裁掉的那四个角要露出桌面，不能露出一块白/黑：清屏色就是
		// Theme.Background，所以这里必须给一个真透明（alpha=0）的背景。
		th.Background = ui.Transparent
		c.SetTheme(th)
	}
	t := c.Theme()
	// 面板统一用一套字体，见 panelFont。
	usePanelFont(t)
	root := c.Root()
	if a.frost != nil {
		// 圆角裁掉的四角露的是窗口自己的底：这块窗口是 DirectComposition 合成的
		// noRedirect 窗口，透明的地方透不出桌面（实测是 #FFFFFF 纯白，桌面是 #FCFCFC），
		// 于是四角成了四块白斑 —— 这正是"圆角还是方的"的来源。
		// 既然透不出去，就别留透明：根上铺一层**桌面本来的颜色**（从快照里取的），
		// 裁掉的角上看到的就是桌面，与真的透出去肉眼无异。
		root.Background(ui.Transparent)
	} else if win.SystemGlass() {
		// 系统 acrylic 那一路（我们自己抓不到全屏快照时走它）：窗口没有 Vibrancy
		// 了，所以这里不能再用 c.Vibrancy() 判断，直接问系统开关。
		root.Background(ui.Transparent)
	} else {
		root.Background(t.Background)
	}
	// 右键菜单选的"删除 / 固定"在这里落地：此时还没开始遍历 results，改它不会越界。
	a.applyPending()

	home := a.query == ""
	if a.query != "" && c.Shortcut(0, ui.KeyEscape) {
		a.query = ""
		a.refilter()
	}
	// Ctrl+K 固定/取消固定选中项（Pane 用 Ctrl+K 打开应用的「更多操作」）。
	if c.Shortcut(ui.Ctrl, ui.KeyK) {
		a.togglePin()
	}
	// Ctrl+Enter 打开所在文件夹（Pane 底栏写明的那一条）。
	if c.Shortcut(ui.Ctrl, ui.KeyEnter) {
		a.openFolder()
	}
	// Ctrl+Shift+C 复制所在目录：Windows 的打开/另存为框里 Ctrl+L → Ctrl+V → 回车，
	// 那个框就直接跳到这个目录（外部程序改不动已经开着的对话框，见 docs/architecture.md）。
	if c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyC) {
		a.copyDir(c)
	}
	if home {
		// 首页方向键不能走 Shortcut：搜索框有焦点时 TextInput 会把左右键吃掉
		// （光标在移动、卡片选中却不动），表现就是"首页左右键失效"。
		// 这里直接读按键按住状态做边沿检测——查询为空时光标动也没意义。
		cols := a.gridCols()
		a.keyStep(c, &a.heldLeft, ui.KeyLeft, -1)
		a.keyStep(c, &a.heldRight, ui.KeyRight, +1)
		a.keyStep(c, &a.heldUp, ui.KeyUp, -cols) // 网格：上下按一行走
		a.keyStep(c, &a.heldDown, ui.KeyDown, +cols)
	} else {
		if c.Shortcut(0, ui.KeyDown) {
			a.choose(a.chosen + 1)
		}
		if c.Shortcut(0, ui.KeyUp) {
			a.choose(a.chosen - 1)
		}
	}
	if c.Shortcut(0, ui.KeyEnter) {
		a.open()
	}

	// DragWindow 挂在根容器上：按住面板任意空白处拖动，整个窗口就跟着走。
	// 子元素（输入框、卡片、按钮）命中优先，所以打字和点击不受影响；
	// 拖到哪、松手后位置记进配置（见 panelGeometryChanged），下次唤出照旧复现。
	// 面板本来就没有标题栏（TitleBarHidden），没有这条就完全拿不动它。
	body := func() {
		// 列表页才有结果列表，所以滚动条的按下区域每帧从零开始：首页那几条
		// 卡片路径上没有列表，留着上一帧的矩形会把空白处的按下误判成"按在滚动条上"。
		a.listBar = ui.Rect{}
		// 固定卡矩形只在本帧有效：首页重建时重新收集，搜索/列表页则清空，
		// 免得旧卡片的坐标把列表页上的按下误判成固定卡。
		a.pinRects = a.pinRects[:0]
		// 顶栏：放大镜 + 大字输入 + 正则开关 + 右端品牌胶囊（Pane 右上角那个 "Pane" 标）。
		ui.Row(c).Padding(16, 20).Gap(14).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, searchIcon).FontSize(20).TextColor(t.TextMuted)
			ph := "搜索应用、文件，或输入命令"
			if a.regexOn {
				ph = "正则匹配文件名，如 ^2026.*\\.log$"
			}
			changed := ui.TextInputBase(c, &a.query).Bind(&a.input).Grow(1).FontSize(20).
				Placeholder(ph).Label("搜索").
				OnSubmit(a.open).Changed()
			a.regexButton(c)
			if changed {
				a.refilter()
			}
			ui.Text(c, "Baozi").FontSize(11).TextColor(t.TextMuted).
				Padding(5, 10).Radius(9).Background(t.Surface)
		})
		ui.Divider(c)
		if home {
			a.homeGrid(c)
		} else {
			a.resultList(c)
		}
		ui.Divider(c)
		a.footer(c)
	}

	// 玻璃底：窗口自己是 noRedirect（DirectComposition）合成的，Win10 的系统磨砂
	// 铺不到它后面，所以背景是一张"抓下来再糊过"的整屏图。
	// 铺法：图按整个虚拟屏的尺寸铺，位置给负的客户区偏移 —— 窗口在图上"取景"。
	// 面板拖到哪，窗口里露出的就是快照里对应的那块桌面；拖动只改这两个偏移，
	// 不重裁、不重模糊、不换纹理，所以跟手。
	// 内容放进 Overlay 才能压在它上面（Absolute 的元素总是画在兄弟之上）。
	if a.frost != nil {
		// 底图（整窗自绘：桌面 + 玻璃 + 边框环，四角靠覆盖率抗锯齿）在这里现算。
		// 放在 view 里而不是挪窗/缩放的回调里，是因为它是"画之前必须就绪"的东西：
		// 每帧布局都会走到这儿，输入没变时 refreshBacking 直接返回（一次比较）。
		a.refreshBacking(false)
		ox, oy := a.glassOffset()
		// 圆角必须挂在**真正画东西的那一层**上：挂在 c.Root() 上时（root 是框架给的
		// 窗口根、自身没有内容）实测裁不掉下面这两层，四角照旧是磨砂底色。
		// 最底一层**不裁圆角**：裁掉的那四个角露出来的就是它 —— 面板外面那块桌面的
		// 颜色。为什么不干脆留透明：这块窗口是 noRedirect（DirectComposition）合成的，
		// 没被内容覆盖的地方由 DWM 填白（把根背景设成纯红，四角实测依旧是 #FFFFFF），
		// 透明在这里换不来桌面，只能把桌面色画上去。
		// 这一层 Fill 铺满整个窗口，而且**挂着 DragWindow**：内容是铺在 Overlay 里的
		// 另一层上，万一那层没盖住窗口的右边或下边（实测底栏那一截就跑到它外面了），
		// 从那块空白按下去时命中的是这条链——它有 DragWindow，面板照样能拖着走。
		// 没有这条兜底，"从下边拖不动面板"就会跟着内容层的尺寸走，改一次尺寸复发一次。
		//
		// **不再 Radius + Clip**：mygo 的 Radius 是 GPU 硬裁剪，没有抗锯齿，而且把
		// 裁掉的像素填成纯白（实测上边缘连续 5 行 #FFFFFF）—— 用户反复说的"漏光"
		// 就是这条白边。四角的形状现在整个烘在底图里（覆盖率抗锯齿），这一层只要
		// 把底图铺满、不裁即可。
		ui.Column(c).Fill().Children(func() {
			if bakedPanel {
				// 底图和客户区同尺寸，1:1 落在 (0,0)，不缩放、不偏移 —— 圆角位置
				// 就是窗口位置，拖动/缩放时只要重新合成一张，弧永远贴着窗边。
				if a.pane.back != nil {
					s := a.glassScale()
					// **按像素 1:1 铺**，一个像素都不缩：底图是 963×771 张物理像素，
					// 就用 963/scale DIP 去画 —— mygo 再乘回 scale 正好是 963 格，
					// 图像一像素对屏幕一像素，覆盖率抗锯齿原样落在屏幕上。
					//
					// 这里曾经写成 (backPixW+2)/s（多铺 2 像素去盖右边/下边那条白线），
					// 代价是整张图被拉成 965 格 → 0.2% 的重采样。直边看不出来，
					// 弧上就是一层糊：每一行都被插值一次，16 档覆盖率被抹成一片，
					// 用户说的"毛边/虚"有一半是这个。右边那条白改为：底图按客户区
					// 的**整数物理像素**尺寸合成（composeBacking 用的是 ClientRectPixels），
					// 画回去时又正好是同一批整数像素，右边/下边一格都不缺。
					dip := float32(a.pane.pw) / s
					dipH := float32(a.pane.ph) / s
					// 往外压一个物理像素：底图四周多烘了一圈（backingPad=1，见
					// refreshBacking），形状本身还在客户区里。mygo 摆放这张图时会
					// 带零点几像素的取整偏差 —— 客户区最上面那一行曾整行没被盖住，
					// 露的是 mygo 清屏的纯白（BAOZI_ROWPROBE 探针实测）。多出来的
					// 这一圈当余量，上下左右无论往哪边取整都盖得住。
					pad := 1 / s
					ui.Image(c, a.pane.back).Absolute().Left(-pad).Top(-pad).
						Size(dip, dipH)
					if p := os.Getenv("BAOZI_BACKDUMP"); p != "" {
						pw, ph := a.panelSize()
						log.Printf("玻璃: 底图铺开 %dx%d px → %.2fx%.2f DIP（面板 %dx%d DIP，scale %.2f）",
							a.pane.pw, a.pane.ph, dip, dipH, pw, ph, s)
					}
				}
			} else {
				// 旧做法（保留作对照）：整屏磨砂图 + 窗口取景偏移 + 四角月牙。
				ui.Image(c, a.frost).Absolute().Left(ox).Top(oy).
					Size(a.frostW, a.frostH)
			}
			ui.Overlay(c, func() {
				// 显式给面板的宽高，别指望 Left/Top/Right/Bottom 把四边撑开：
				// Absolute 元素的高度实测是**按内容算**的（Right/Bottom 不起作用），
				// 底栏那一截就跑到了这层外面 —— 拖动的命中链落到底下那层面板底色上，
				// 而它没有 DragWindow，于是"从下边按下去拖不动面板"。
				// 宽度同理：内容不够宽时右边也是同样一条死区。
				pw, ph := a.panelSize()
				// 记住边线色：四角的月牙会把圆弧那一段边一起画（带超采样），见 glass.go。
				a.borderColor, a.borderSet = t.Border, true
				// 整圈边分两段画：直边用四条细线（下面），圆角段由四角的月牙画。
				//
				// 内容层现在**必须裁圆角**：底栏、卡片这些不透明底色是矩形的，
				// 不裁就会一直铺到窗口四角，把底图烘出来的弧和弧外的桌面整个盖成
				// 直角（截图里正是"只有上面两个角是圆的，下面俩是方的"）。
				// mygo 的圆角 clip 走的是覆盖率裁剪（internal/raster/clipmask.go），
				// 不是 1 位掩码，所以这一刀本身也有抗锯齿。
				//
				// 半径比面板小 1 DIP：让出最外那一像素给底图烘的边框环 —— 内容铺到
				// 边上就会把环盖掉（以前"圆角那段没有边框"就是这么来的）。
				ui.Column(c).Absolute().Left(0).Top(0).
					Size(float32(pw), float32(ph)).
					Radius(float32(glassClipRadius)).Clip().
					Children(body)
				// 四条直边。直边段和弧上的边线同色同宽，拼成一整圈框。
				//
				// 起点不能精确取弧半径（r0 = glassRadius）。精确相接时，直边线的
				// 终点由"面板 DIP 宽度"算出来，弧的端点却落在"窗口像素宽度"上，
				// 两者差 1~2 像素，接缝处就露出一小段缝：看上去是一条直边线
				// 孤零零地断在角旁边（截图实测 71×58 那块，底边线画到 x=37，
				// x=38-39 空了两像素，弧从 x=40 才接上）。
				//
				// 所以两端各多画 2 DIP，主动压进弧段。压进弧内的部分和弧上的边线
				// 同色，看不出重叠；压到弧外的那一点点由最后绘制的四角月牙盖住
				// （月牙的弧外区域铺的是该角的真实桌面色，alpha 不透明）。
				const edgeDip float32 = 1
				const edgeOverlap float32 = 1 // 内侧柔化：弧带是"1 DIP 满覆盖 + 内外各 1 DIP 柔化"，视觉宽度约 2 DIP；
				// 直边线只是一条 1 DIP 的实心矩形，没有柔化。两者在弧的端点接上时，
				// 接口处宽度从 1 DIP 突变到 2 DIP，看上去就是"一条细直线接到一段较粗的
				// 弧上"——用户描出来的那个 Γ，左边距面板右边正好 30px（= 弧半径）、
				// 上边距底边 29px，标的就是这个交界点。给直边线内侧补一条同宽的
				// 30% 边线，两边视觉宽度一致，接口就平了。
				edgeSoft := t.Border.Alpha(0.3)
				// 所有边线都按**物理像素**定位，而不是按 DIP 面板宽高：
				// 面板是 909 DIP × 1.25 = 1136.25 物理像素，窗口实际只有 1136 个像素宽
				// —— 用 909 DIP 当右边界，那条 1 DIP 的边线就压在半像素上：1135 只盖到
				// 0.75、1136 那一格落在窗口外，右边和下边于是只剩半条线，旁边还露出
				// 一格亮底（截图实测右侧 #F5F5F5、下侧 #FFFFFF），看上去就是"边框里面
				// 还有一条线"。上边、左边从 0 起算，不存在这个偏差。
				// 这里把"一像素"换算成 DIP（1/scale）来画，四条边都正好铺满一格。
				scale := float32(a.glassScale())
				if scale <= 0 {
					scale = 1
				}
				one := 1 / scale // 正好一个物理像素的 DIP 宽
				soft := one * 0.4
				// 面板右/下边在 DIP 坐标里的真实位置：物理像素宽高除以缩放，可能不是整数
				wDip := float32(math.Round(float64(pw)*float64(scale))) / scale
				hDip := float32(math.Round(float64(ph)*float64(scale))) / scale
				r0 := float32(glassRadius) - edgeOverlap
				innerW := wDip - 2*r0
				innerH := hDip - 2*r0
				if panelBorder && !bakedPanel {
					ui.Column(c).Absolute().Left(r0).Top(0).
						Size(innerW, one).Background(t.Border)
					ui.Column(c).Absolute().Left(r0).Top(one).
						Size(innerW, soft).Background(edgeSoft)
					ui.Column(c).Absolute().Left(r0).Top(hDip-one).
						Size(innerW, one).Background(t.Border)
					ui.Column(c).Absolute().Left(r0).Top(hDip-one-soft).
						Size(innerW, soft).Background(edgeSoft)
					ui.Column(c).Absolute().Left(0).Top(r0).
						Size(one, innerH).Background(t.Border)
					ui.Column(c).Absolute().Left(one).Top(r0).
						Size(soft, innerH).Background(edgeSoft)
					ui.Column(c).Absolute().Left(wDip-one).Top(r0).
						Size(one, innerH).Background(t.Border)
					ui.Column(c).Absolute().Left(wDip-one-soft).Top(r0).
						Size(soft, innerH).Background(edgeSoft)
				}
			})
			// 四角的月牙最后画，压住磨砂图、底色和边线：弧外是该角真实的桌面色，
			// 弧上按覆盖率渐隐，弧内透明露出玻璃 —— 见 glass.go 的
			// buildCornerBitmaps，那里写了为什么 Win10 只能这么画。
			//
			// 位置必须落在**整数物理像素**上。用面板的 DIP 宽高去算偏移是不行的：
			// 805 DIP × 1.25 = 1006.25，右上、右下两张图就落在半像素的位置，GPU
			// 采样渗到透明边缘，角上漏出一两个白点（左上、左下从 0 起步，所以没事，
			// 这就是四个角只有斜对的两个露白的原因）。这里按窗口客户区的真实像素
			// 宽高算偏移，乘回缩放之后正好是整数。
			// 拖动/缩放期间也画：那时阴影层是收起的（见 OnResize），四角就只剩窗口
			// 区域那道 1 位硬边 —— 右边和下边正在跟着手柄变，台阶看得最清，用户看到的
			// 就是"右上、右下变成方角"。静止时仍然交给阴影层（它画的弧最干净，
			// 两套一起画反而会叠出一道白边）。
			if !bakedPanel && (UseCornerBitmaps || a.dragging()) && a.cornerPx > 0 {
				s := a.glassScale()
				pw, ph := a.panelSize()
				cw, ch := int(math.Round(float64(pw)*float64(s))), int(math.Round(float64(ph)*float64(s)))
				if hwnd := a.panel.NativeHandle(); hwnd != 0 {
					if _, _, w, h := win.WindowRectPixels(hwnd); w > 0 && h > 0 {
						cw, ch = w, h
					}
				}
				dip := float32(a.cornerPx) / s
				leftFar := float32(cw-a.cornerPx) / s
				topFar := float32(ch-a.cornerPx) / s
				for i := 0; i < 4; i++ {
					img := a.cornerBitmaps[i]
					if img == nil {
						continue
					}
					x, y := float32(0), float32(0)
					if i == 1 || i == 3 {
						x = leftFar
					}
					if i == 2 || i == 3 {
						y = topFar
					}
					ui.Image(c, img).Absolute().Left(x).Top(y).Size(dip, dip)
				}
			}
		})
		return
	}
	ui.Column(c).Fill().Children(body)
}

// regexButton 是搜索框右边那个 ".*" 切换按钮：点一下进正则模式，再点回普通搜索。
// 开着时整块用 Accent 底标出来 —— 正则模式和普通搜索结果差得很远，
// 状态必须一眼可辨，否则用户会以为"搜索坏了"。
func (a *application) regexButton(c *ui.Context) {
	t := c.Theme()
	b := ui.Button(c, ".*").FontSize(12).Padding(3, 9).Radius(7).
		Tooltip("正则模式：只按正则精确匹配文件名，不走拼音与模糊")
	if a.regexOn {
		b.TextColor(t.Text).Background(t.Accent.Alpha(0.28)).
			BorderWidth(1).BorderColor(t.Accent.Alpha(0.6))
	} else {
		b.TextColor(t.TextMuted).Background(t.Surface)
	}
	if !b.Clicked() {
		return
	}
	a.regexOn = !a.regexOn
	a.patternErr = ""
	if a.regexOn {
		a.setStatus("正则模式：只精确匹配文件名")
	} else {
		a.setStatus("已回到普通搜索")
	}
	// 鼠标点过按钮之后焦点可能从输入框跑掉，用户接着就要敲正则——把焦点还回去。
	if a.panel != nil {
		a.panel.Update(func() { a.input.Focus() })
	}
	a.refilter()
}

// homeGrid 是空查询时的首页：「最近使用」在上、「已固定」在下两段网格卡片。
// 顺序照 Pane——常用的贴手，固定的沉底。
func (a *application) homeGrid(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Grow(1).Children(func() {
		if len(a.results) == 0 {
			ui.Text(c, "开始输入以搜索文件和应用；打开过的项目会留在这里").
				TextColor(t.TextMuted).Padding(20)
			return
		}
		section := func(title string) {
			ui.Text(c, title).FontSize(11).TextColor(t.TextMuted).Padding(10, 18, 3, 18)
		}
		// 不用 Grid：它的列宽是均分的，相同宽度下每列会比卡片宽、卡片居中会左右各空一截，
		// 看着"占位很大"。这里自己拼行：卡片宽度走 cellWidth()、间距 cardGap，
		// 一排几个由面板宽度算（gridCols），面板调宽调窄时列数自动跟着变。
		cols := a.gridCols()
		cards := func(from, to int) {
			ui.Column(c).Gap(cardRowGap).Padding(2, cardGutter).Children(func() {
				for start := from; start < to; start += cols {
					end := start + cols
					if end > to {
						end = to
					}
					// 顶部对齐是硬要求：卡片高度由"名字一行还是两行"决定，
					// 默认的居中会让名字短的卡片整体往下沉，于是图标顶不齐、
					// 标题也不在同一行（用户就是拿这个截图来指正的）。
					row := ui.Row(c).Gap(cardGap).AlignItems(ui.Start).Children(func() {
						for i := start; i < end; i++ {
							a.card(c, i)
						}
					})
					// 拖动中的卡片要压住别的行：跨行拖的时候，后面的行是按
					// 文档顺序画的，不把整行提到前面，被拖的卡片会被它们盖住。
					if a.pinDragActive && a.pinDragFrom >= start && a.pinDragFrom < end {
						row.ZIndex(1)
					}
				}
			})
		}
		recentTo := len(a.results) - a.pinnedCount
		if recentTo > 0 {
			section("最近使用")
			cards(0, recentTo)
		}
		// 这行弹性空白把「已固定」压到面板最底下（Column 的子项不会自己撑开）。
		// 内容多到放不下时它会被压成 0，固定段照样紧接其后。
		ui.Row(c).Grow(1)
		if a.pinnedCount > 0 {
			section("已固定")
			cards(recentTo, len(a.results))
		}
	})
}

// wrapName 把名字折成卡片能放下的样子，按"字宽"而不是"字符数"算。
// 卡片内容宽 76 DIP、名字 10.5 号：中日韩字符约 10.5 DIP，拉丁字符约 5.5 DIP。
// 为什么必须自己折：英文长文件名（SnowShot_Video_2026-10-29_13-31-04.mp4）没有
// 空格可断，mygo 不会按字符硬断，只会横向溢出到隔壁卡片上叠成一团。
// 断行优先落在 _ - . 空格 这些分隔符后面，切掉的部分不超过行宽的 45%，
// 这样 "SnowShot_Video_" 不会被拆成 "SnowShot_Vide" + "o_..." 那种样子。
// 满 lines 行还放不下才加省略号；完整名字另有 Tooltip 兜底。
func wrapName(name string, width float32, lines int) string {
	const cjkWidth, latinWidth = 10.5, 5.5
	wOf := func(r rune) float32 {
		if r > 0x2e80 {
			return cjkWidth
		}
		return latinWidth
	}
	isBreak := func(r rune) bool {
		switch r {
		case ' ', '_', '-', '.', '/', '\\':
			return true
		}
		return false
	}
	runes := []rune(name)
	var parts []string
	start := 0
	for start < len(runes) {
		var used float32
		end := start
		lastBreak := -1
		for end < len(runes) {
			w := wOf(runes[end])
			if used+w > width {
				break
			}
			used += w
			end++
			if isBreak(runes[end-1]) {
				lastBreak = end
			}
		}
		if end >= len(runes) {
			parts = append(parts, string(runes[start:]))
			break
		}
		if len(parts) == lines-1 {
			parts = append(parts, string(runes[start:end])+"…")
			break
		}
		cut := end
		// 分隔符断行只损失一两格宽度时才值得：回退太多会让最后一行提前截断，
		// 反而少显示字（37 个字符的文件名要在三行里装完，一格都不能浪费）。
		if lastBreak > start && end-lastBreak <= 2 {
			cut = lastBreak
		}
		parts = append(parts, string(runes[start:cut]))
		start = cut
	}
	return strings.Join(parts, "\n")
}

// card 是首页网格里的一格：圆角图标块 + 居中的名字。
// 尺寸照启动器那种"大图标 + 名字"的样子：卡片 84 宽，图标 55（跟着设置缩放），
// 名字 12 号、最多两行。不再给图标套浅灰方块——那层底色会把彩色图标压小一圈。
// 卡片宽不跟着图标变：哪怕图标调到 64，84 宽也还放得下，首页保持整齐。
func (a *application) card(c *ui.Context, i int) {
	r := a.results[i]
	t := c.Theme()
	// 选中 / 悬停都是一个"圆角底色块"，**不描边**：参照用户给的启动器截图，
	// 那边选中的那张是整块深灰圆角底，没有一个框套在外面。
	// 之前用「浅底 + 蓝细边」，边框在小图标旁边显得琐碎，用户明确说不要。
	bg := ui.Transparent
	if i == a.chosen {
		bg = t.SurfaceHover
	}
	// 「已固定」区的卡片可以拖着重排：拖动中的那张换成强调色底，让人知道"手上有东西"。
	pinned := i >= len(a.results)-a.pinnedCount
	dragging := pinned && a.pinDragActive && a.pinDragFrom == i
	if dragging {
		bg = t.Accent.Alpha(0.22)
	}
	col := ui.Column(c).Width(a.cellWidth()).Padding(6, 4).Gap(6).AlignItems(ui.Center).Radius(12).
		Background(bg).
		OnClick(func() {
			a.chosen = i
			a.open()
		}).
		ContextMenu(func(m *ui.Menu) { a.itemMenu(m, i) }).
		Children(func() {
			// 底色块铺满整张卡（图标 + 名字都在里面），只是**不描边**：照用户给的启动器
			// 截图，选中那张是一整块圆角底，名字也在块里。之前把底色只留给图标、名字晾在
			// 外面，悬停时看着像"标题没被选中"。
			//
			// 图标盒固定高度是为了让位图和线框的图标顶边、名字基线都对得齐（线框 SVG 的
			// 图形只占 24 网格的 18/24，不套盒子会小一圈、上下错开）。
			ui.Row(c).Width(a.cardIcon() + 8).Height(a.cardIcon()).
				AlignItems(ui.Center).Justify(ui.Center).Children(func() {
				// 图标直接铺在卡上，不再套那个浅灰方块：应用/程序的彩色图标本身就是
				// 辨识度，套个小方块只会把它压小、还多出一圈底色（见 cardIcon）。
				a.resultIcon(c, r, a.cardIcon())
			})
			// 名字最多两行：一行放得下就只占一行，超了才往下续第二行。
			ui.Text(c, wrapName(r.Name, a.cellWidth()-12, 2)).FontSize(12).TextColor(t.Text).
				TextAlign(ui.Center).MaxLines(2).LineHeight(1.2).Tooltip(r.Name)
		})
	// 鼠标停在哪张卡上，就等于选中哪张（Enter / 右键 / 底栏都作用在它身上）。
	a.hoverSelect(col, i)
	if !pinned {
		return
	}
	// 拖动本身不改数据：松手时才在下一帧的 applyPending 里重排（渲染途中不能改 results）。
	col = col.Draggable(a.pinDragPoint(i), ui.DraggableOptions{})
	if dragging {
		// 同一行里也要压住右边的卡片；只设父行不够。
		col = col.ZIndex(1)
	}
	a.trackPinDrag(i, col)
	h := a.pinHandle(i)
	col = col.Bind(h)
	// 这里读到的是上一帧提交后的矩形，正是用户此刻看到的那张卡片。
	a.pinRects = append(a.pinRects, h.Bounds(c))
}

// hoverSelect 让"指针停在哪张卡上"就等于选中哪张：鼠标一移过去选中就跟着走，
// 之后的 Enter、右键、底栏提示都作用在那张卡上。
//
// 关键在"只在指针真的动了的时候接管"。否则鼠标停在 A 上、用户按 ↓ 想选 B，
// 下一帧 hover 又把人拽回 A——键盘就彻底失灵了。所以比的是**窗口坐标**，
// 不是元素自己的相对坐标（那两张卡可能恰好同值）。
func (a *application) hoverSelect(el ui.Element, i int) {
	x, y, over := el.PointerPosition()
	if !over {
		return
	}
	bounds := el.Bounds()
	chosen, point, known := hoverTarget(a.hoverPoint, a.hoverKnown,
		ui.Point{X: bounds.X + x, Y: bounds.Y + y}, a.chosen, i)
	a.hoverPoint, a.hoverKnown = point, known
	if chosen != a.chosen {
		a.chosen = chosen
	}
}

// hoverTarget 是 hoverSelect 的纯逻辑部分，单独拿出来好测：
// 指针不在元素上、或者位置没变，就不接管选中。
func hoverTarget(prev ui.Point, known bool, abs ui.Point, chosen, i int) (int, ui.Point, bool) {
	if !known {
		return i, abs, true
	}
	if abs == prev {
		return chosen, prev, true
	}
	return i, abs, true
}

// itemMenu 是首页卡片和结果行共用的右键菜单：固定 / 取消固定、打开所在文件夹、从历史删除。
func (a *application) itemMenu(m *ui.Menu, i int) {
	if i < 0 || i >= len(a.results) {
		return
	}
	r := a.results[i]
	key := resultKey(r)
	if a.hist.IsPinned(key) {
		if m.Item("取消固定").Chosen() {
			a.pendingOp, a.pendingIdx = "unpin", i
		}
	} else {
		if m.Item("固定到首页").Shortcut(ui.Ctrl, ui.KeyK).Chosen() {
			a.pendingOp, a.pendingIdx = "pin", i
		}
	}
	// 命令类结果、shell: 这类没有真实路径，灰掉而不是点了才说"没有文件夹"。
	// 和 Ctrl+Enter 是同一个动作，菜单里把快捷键也标出来，省得记两套。
	folder := m.Item("打开所在文件夹").Shortcut(ui.Ctrl, ui.KeyEnter)
	if !hasContainingFolder(r) {
		folder.Disabled(true)
	} else if folder.Chosen() {
		a.pendingOp, a.pendingIdx = "folder", i
	}
	m.Separator()
	if m.Item("从历史删除").Danger().Chosen() {
		a.pendingOp, a.pendingIdx = "delete", i
	}
}

// hasContainingFolder 说这一条结果有没有真实的所在文件夹。
func hasContainingFolder(r search.Result) bool {
	return r.Kind != search.KindCommand && r.Path != "" && !strings.HasPrefix(r.Path, "shell:")
}

// resultList 是有查询时的结果列表：单列，行式。
func (a *application) resultList(c *ui.Context) {
	t := c.Theme()
	// 不把 Selected 交给 List：List 会用整块 accent 填充画选中行，
	// 我们要的是 Pane 那种「浅色胶囊 + 细边」，选中态由 row 自己画。
	a.list.Selected = nil
	a.list.Key = func(i int) any { return a.results[i].Path + a.results[i].Command }
	a.list.Label = func(i int) string { return a.results[i].Name }
	list := ui.List(c, &a.list, len(a.results), func(i int) { a.row(c, i) }).Grow(1).Padding(8).Children(func() {
		if len(a.results) == 0 {
			switch {
			case a.regexOn && a.patternErr != "":
				// 正则写错了要指出来，否则"搜不到"像是功能没生效
				ui.Text(c, "正则无效："+a.patternErr).TextColor(t.TextMuted).Padding(18)
			case a.regexOn:
				ui.Text(c, "没有文件名命中这个正则").TextColor(t.TextMuted).Padding(18)
			case search.HasGlob(a.query) && a.patternErr != "":
				ui.Text(c, "通配模式无效："+a.patternErr).TextColor(t.TextMuted).Padding(18)
			case search.HasGlob(a.query):
				ui.Text(c, "没有文件名匹配这个通配模式").TextColor(t.TextMuted).Padding(18)
			default:
				ui.Text(c, "没有匹配项").TextColor(t.TextMuted).Padding(18)
			}
		}
	})
	if list.Submitted() {
		a.open()
	}
	// 竖滚动条的按下区域记下来给窗口拖动用（见 scrollBarPressHit）。
	// 读的是**上一帧提交后**的矩形：和固定卡一样，这一帧刚建的 Element 还没有坐标。
	a.listBar = listBarStrip(a.list.Bounds(c), c.Theme())
}

// footer 是底栏：左边说选中项会做什么，右边用键帽写清 Enter / Ctrl+Enter / Ctrl+K。
//
// 键帽（keyCap）只是提示，故意画得很轻：浅灰底、无边框、灰色小字。
// 「设置」「互传」是能点的按钮，必须和键帽分开——早先它们也是浅灰底的小块，
// 结果跟键帽长得一模一样，用户根本看不出那是入口。
func (a *application) footer(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Padding(9, 18).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, a.footerText()).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine()
		a.keyCap(c, "Enter", "打开")
		a.keyCap(c, "Ctrl+Enter", "文件夹")
		a.keyCap(c, "Ctrl+Shift+C", "复制路径")
		a.keyCap(c, "Ctrl+K", "固定")
		a.keyCap(c, "右键", "删除/固定")
		// 跟键帽之间空开一截，一眼能看出"提示"到这儿结束了，后面是两个入口。
		ui.Row(c).Width(14).Height(1)
		// 入口按钮带图标：文字按钮和键帽（浅灰小块）本来就容易混，加个图形立刻区分开，
		// 也顺手把"哪个是设置、哪个是互传"从读字变成看图。
		//
		// 图标必须 Shrink(0)：底栏这一行里键帽占地方，空间不够时 flex 会把可压缩的
		// 子元素一路压到 0 宽——图片就是那个最先被压没的东西（文字有 min-content 兜着，
		// 图片没有），结果就是按钮只剩字、图标人间蒸发。
		// 按钮圆角：面板四角是 16 DIP 的弧，按钮却一直是主题默认的 3~4px，
		// 两者并排时按钮那个几乎成直角的小方角特别扎眼（截图里「互传」按钮
		// 右下角 x=36、y=32 那个交叉点就是这样）。取 8 DIP，约为按钮高度的
		// 三分之一，看得出圆、又不至于圆成胶囊。
		const footerRadius float32 = 8
		if btn := ui.Button(c, "").Shrink(0).Radius(footerRadius).Children(func() {
			ui.Image(c, footerIcon(iconGear, t.Text, iconBitmapSize)).Size(16, 16).Shrink(0)
			ui.Text(c, "设置").FontSize(12)
		}); btn.Clicked() {
			a.openSettings()
		}
		if btn := ui.Button(c, "").Shrink(0).Radius(footerRadius).Children(func() {
			ui.Image(c, footerIcon(iconSwitch, t.Text, iconBitmapSize)).Size(16, 16).Shrink(0)
			ui.Text(c, "互传").FontSize(12)
		}); btn.Clicked() {
			a.openShare()
		}
	})
}

func (a *application) footerText() string {
	if a.chosen >= 0 && a.chosen < len(a.results) {
		r := a.results[a.chosen]
		if r.Path != "" && a.hist.IsPinned(r.Path) {
			return "已固定 · " + r.Name
		}
		return "打开 " + r.Name
	}
	// 有新版本就顺嘴提一句，位置在本来就要显示状态的那一格 —— 不新增控件、
	// 不抢焦点、也不弹任何东西（检查是后台做的，这里只是把结果念出来）。
	if tag := a.latestTag(); tag != "" {
		if a.status == "" {
			return "有新版本 " + tag + " · 设置 → 关于"
		}
		return a.status + " · 有新版本 " + tag
	}
	return a.status
}

func (a *application) keyCap(c *ui.Context, key, what string) {
	t := c.Theme()
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Text(c, key).FontSize(10).TextColor(t.TextMuted).
			Padding(3, 6).Radius(5).Background(t.Surface)
		ui.Text(c, what).FontSize(10).TextColor(t.TextMuted)
	})
}

func (a *application) row(c *ui.Context, i int) {
	r := a.results[i]
	t := c.Theme()
	// Pane 的选中态是"整行浅色胶囊 + 细边"，不是整块高饱和填充。
	bg, border := ui.Transparent, ui.Transparent
	if i == a.chosen {
		bg = t.Accent.Alpha(0.16)
		border = t.Accent.Alpha(0.5)
	}
	row := ui.Row(c).Padding(9, 12).MarginY(2).Gap(12).AlignItems(ui.Center).Radius(10).
		Background(bg).BorderWidth(1).BorderColor(border).
		OnClick(func() {
			// 鼠标点一下就打开：之前这里没有任何点击处理，点了像没反应。
			a.chosen = i
			a.open()
		}).
		ContextMenu(func(m *ui.Menu) { a.itemMenu(m, i) }).
		Children(func() {
			a.resultIcon(c, r, a.iconSize())
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, r.Name).TextColor(t.Text).FontSize(14).Bold().SingleLine().Tooltip(r.Name)
				ui.Text(c, r.Hint).FontSize(11).TextColor(t.TextMuted).SingleLine()
			})
			ui.Text(c, badge(r)).FontSize(10).TextColor(t.TextMuted)
		})
	// 悬停即选中，和首页卡片一致：鼠标扫过哪一行，Enter 就打开哪一行。
	a.hoverSelect(row, i)
}

// iconSize 是结果行左侧图标的边长。没在设置里指定就跟着系统缩放走
// （100% → 22、125% → 27.5），这块逻辑在 config.Search.IconSizeAt 里，
// 因为它同时要被设置页的"当前会显示多大"预览用到。
func (a *application) iconSize() float32 {
	return a.cfg.IconSizeAt(a.scale)
}

// gridCols 是首页网格一排能放几张卡：由当前面板宽度算出，宽度一改列数跟着变。
// 方向键"上下走一行"用的也是它，所以它和 homeGrid 的排布必须同源。
func (a *application) gridCols() int {
	w, _ := a.panelSize()
	n := (w - 2*cardGutter + cardGap) / int(a.cellWidth()+cardGap)
	return max(1, n)
}

// panelSize 返回面板当前的实际宽高（DIP）。
//
// 有真窗口就问它——用户拖着边缘把面板拉宽之后，列数要立刻跟着变；
// 视图测试里没有窗口（a.panel 为 nil），退回配置里的初始尺寸。
func (a *application) panelSize() (int, int) {
	if a.panel != nil {
		if w, h := a.panel.Size(); w > 0 && h > 0 {
			return w, h
		}
	}
	return a.cfg.PanelSize()
}

// cardIcon 是首页卡片里图标的边长。卡片里图标按"结果行图标 ×2.2"放大
// ——125% 缩放下结果行是 27.5、卡片就是 60.5，接近启动器那种"图标占满整格"的比例
// （再大名字就只剩一行了）。只夹下限：太小认不出是什么。
//
// 上限不夹：图标大小是倍数之后，200% 在 4K 屏（系统 200% 缩放）会算到 88 像素、
// 卡片跟着长到 104 宽，一排自然少放几张——用户把倍数调大，要的就是这个大。
// 夹一个上限等于把"倍数"这个说法废掉一半。
func (a *application) cardIcon() float32 {
	icon := a.iconSize() * 2.2
	if icon < 32 {
		icon = 32
	}
	return icon
}

// cellWidth 是首页一张卡（也是选中时那块圆角底）的宽度：至少 cardWidthMin
// （要撑住"远程桌面连接"这种六字名），图标放大时按图标宽度加一圈留白。
// 网格列数、拖动换位的步长都从它来，三者必须同源。
func (a *application) cellWidth() float32 {
	w := a.cardIcon() + 16
	if w < cardWidthMin {
		w = cardWidthMin
	}
	return w
}

// resultIcon 画结果左侧的类型图标：文件/目录/命令用单色线框（icon.go），
// 应用用系统的彩色图标——应用的图标本身就是辨识度，换成线框反而认不出。
// .exe / .lnk 虽然记成文件，但它本身就是个程序，同样走彩色图标：线框认不出是哪个程序。
// size 是图标显示边长：行里 22，首页卡片 23。
func (a *application) resultIcon(c *ui.Context, r search.Result, size float32) {
	t := c.Theme()
	if g := glyphFor(r); g != nil {
		if isProgram(r) {
			if bm := a.appIcon(r, int(size*2.2)); bm != nil {
				ui.Image(c, bm).Size(size, size)
				return
			}
		} else if thumbable(r) {
			if bm := a.thumbIcon(r, size); bm != nil {
				ui.Image(c, bm).Size(size, size)
				return
			}
		}
		// 线框图标用 Size 而不是 FontSize：FontSize 给的是"字号"，而 SVG 24 网格里
		// 图形只占约 18/24，落到 60 的卡片里会显得比彩色位图小一圈、顶边也对不齐。
		// 给死尺寸后图形按图形填满这个方块，和位图同高。
		ui.Icon(c, g).Size(size, size).TextColor(t.TextMuted)
		return
	}
	// 位图按 2.2 倍请求再缩下来：系统图标四周那圈透明边裁掉后剩下的内容往往只有
	// 原图的六七成，按 1.6 倍要图裁完就不够 26 用了，会糊。
	if bm := a.appIcon(r, int(size*2.2)); bm != nil {
		ui.Image(c, bm).Size(size, size)
		return
	}
	ui.Icon(c, glyphFile).Size(size, size).TextColor(t.TextMuted)
}

// isProgram 判断这个"文件"其实是个可执行程序：.exe/.lnk/.msi/.scr/.com。
// 这类条目在历史里记成文件，但用户期望看到的是它自己的图标。
// .bat/.cmd 不算：它们的 shell 图标是通用的齿轮，还不如统一走线框。
func isProgram(r search.Result) bool {
	if r.Kind != search.KindFile {
		return false
	}
	switch strings.ToLower(r.Ext) {
	case ".exe", ".lnk", ".msi", ".scr", ".com":
		return true
	}
	return false
}

func badge(r search.Result) string {
	switch r.Kind {
	case search.KindApp:
		return "应用"
	case search.KindDir:
		return "目录"
	case search.KindCommand:
		return "命令"
	}
	if r.Ext != "" {
		return strings.TrimPrefix(r.Ext, ".")
	}
	return "文件"
}

// appIcon 取应用的彩色图标并缓存。只有应用还向 Shell 要图：它的 Path 是
// shell:AppsFolder\<AUMID>，按扩展名取会拿到一个无关的通用图标（"图标和名称
// 对不上"就是这么来的）。文件与目录已改成 icon.go 里的单色线框图标，
// 不再走这里——少了几万次 COM 调用，也不会再冒出深色底上的白色纸片。
func (a *application) appIcon(r search.Result, px int) *ui.Bitmap {
	scale := a.scale
	if scale <= 0 {
		scale = 1
	}
	px = int(float32(px) * scale)
	key := fmt.Sprintf("app:%d:%s", px, r.Path)
	// 先看缓存：已经解码好的图标不依赖 Shell，离线（视图测试）也能照常画出来。
	if b, ok := a.icons[key]; ok {
		return b
	}
	if a.offline {
		return nil
	}
	data, _ := mygo.Shell.FileIcon(iconPath(r.Path), px)
	if trimmed, err := win.TrimIcon(data); err == nil {
		data = trimmed
	}
	bitmap, _ := ui.DecodeBitmap(data)
	// 失败不写缓存：否则这张图标在本进程里永远是空的，看着就是"这程序没图标"。
	if bitmap != nil {
		a.icons[key] = bitmap
	}
	return bitmap
}

// iconPath 把应用条目的 Path 换成"取图标该用的那个路径"。
//
// 正常的应用条目 Path 是 shell:AppsFolder\<AUMID>，这个必须原样交给 Shell
// （当成文件路径按扩展名取会拿到一个不相干的通用图标）。
// 但历史里真的出现过 shell:AppsFolder\D:\Games\x\PCLauncher.exe 这种写法——
// 前缀后面跟的是一个真路径。这时剥掉前缀按普通文件取图最靠谱，
// 否则 Shell 会把它当一个不存在的 AUMID 去解析。
func iconPath(p string) string {
	const prefix = `shell:AppsFolder\`
	if len(p) > len(prefix) && strings.EqualFold(p[:len(prefix)], prefix) {
		rest := p[len(prefix):]
		// 盘符开头的才是真路径；其余一律当 AUMID 交给 Shell。
		if len(rest) >= 3 && rest[1] == ':' && (rest[2] == '\\' || rest[2] == '/') {
			return rest
		}
	}
	return p
}

// thumbable 判断这条结果要不要显示内容缩略图：图片、视频、PDF。
// 其它文档不取 —— .txt/.docx 的缩略图多半是白纸或空白页，还不如统一线框好认。
func thumbable(r search.Result) bool {
	if r.Kind != search.KindFile || r.Path == "" {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(r.Ext, "."))
	return imageExts[ext] || videoExts[ext] || ext == "pdf"
}

// thumbIcon 返回这条结果的缩略图；还没取到就先返回 nil（本帧画线框），
// 并在后台下单，取回后经 panel.Update 重绘。
//
// 为什么异步：冷态一张 19ms、最慢 80ms，一屏十几行同步取就是几百毫秒的白屏。
// 宁可先看到线框图标，也别为了小图把面板唤出的手感拖慢。
func (a *application) thumbIcon(r search.Result, size float32) *ui.Bitmap {
	// 与 appIcon 同样按 2.2 倍请求：22 逻辑 px 的方块，2.2 倍后缩下来才不糊。
	px := int(size * 2.2)
	if s := a.scale; s > 1 {
		px = int(float32(px) * s)
	}
	key := fmt.Sprintf("thumb:%d:%s", px, r.Path)
	// 先看缓存：已经取到的图不依赖 Shell，离线（视图测试）也能照常画出来。
	if bm, ok := a.icons[key]; ok {
		return bm // ok 但 bm 为 nil：这张系统给不出缩略图，别再反复试
	}
	if a.offline || !a.cfg.ThumbnailsOn() {
		return nil
	}
	if a.thumbAsk == nil {
		a.thumbAsk = map[string]bool{}
		a.thumbSem = make(chan struct{}, 4)
	}
	if a.thumbAsk[key] {
		return nil
	}
	select {
	case a.thumbSem <- struct{}{}:
		a.thumbAsk[key] = true
		go a.loadThumb(key, r.Path, px)
	default:
		// 四个在飞已经满了：这次不排队，等下一帧有空位再来
	}
	return nil
}

// loadThumb 在后台线程取一张缩略图。COM 的 STA 与线程绑定，所以进去第一件事
// 是把线程钉住（win.Thumbnail 内部自己 LockOSThread，不用这里管）。
func (a *application) loadThumb(key, path string, px int) {
	defer func() { <-a.thumbSem }()
	data, err := win.Thumbnail(path, px)
	if err != nil {
		a.writeIcon(key, nil)
		return
	}
	bm, _ := ui.DecodeBitmap(data)
	a.writeIcon(key, bm) // 解码失败时 bm 为 nil，同样记成"取不到"
}

// writeIcon 把异步取回的图塞进缓存并在 UI 线程重绘。
// icons 只在 UI 线程读写（渲染、以及这里的 Update 回调），所以不用加锁。
func (a *application) writeIcon(key string, bm *ui.Bitmap) {
	stash := func() {
		a.icons[key] = bm
		// 滚过几百张图之后缓存会一直涨；超过上限就把缩略图整批丢掉（程序图标留着）。
		// 不做 LRU：缩略图重建只要 1ms，为省这点钱维护链表不值。
		if len(a.icons) > 900 {
			a.dropThumbs()
		}
	}
	if a.panel == nil {
		stash()
		return
	}
	a.panel.Update(stash)
}

func (a *application) dropThumbs() {
	for k := range a.icons {
		if strings.HasPrefix(k, "thumb:") {
			delete(a.icons, k)
		}
	}
	for k := range a.thumbAsk {
		delete(a.thumbAsk, k)
	}
}

// screenScale 取所有显示器里最大的缩放比例（图标要按物理像素请求，缩下来才清晰）。
func screenScale() float32 {
	scale := float32(1)
	for _, d := range mygo.Screen.Displays() {
		scale = max(scale, float32(d.ScaleFactor))
	}
	return scale
}

// refilter 处理用户输入：空查询给历史，否则走搜索。
func (a *application) refilter() {
	q := strings.TrimSpace(a.query)
	if debugKeys {
		log.Printf("query: %q", q)
	}
	if q == "" {
		a.results = a.historyResults(12)
		a.choose(0)
		return
	}
	// 输入即搜索：放到后台，避免几百万条目卡住 UI；结果回来时校验查询没变。
	go func() {
		res := a.searchAll(q)
		a.panel.Update(func() {
			if strings.TrimSpace(a.query) == q {
				a.results = res
				a.choose(0)
				if debugKeys {
					for i := 0; i < len(res) && i < 5; i++ {
						log.Printf("  #%d %s (kind=%d, acc=%.2f, type=%.2f, score=%.3f)",
							i+1, res[i].Name, res[i].Kind, res[i].Accuracy, res[i].TypeScore, res[i].Score)
					}
					var ac, fc, dc, cc int
					for _, r := range res {
						switch r.Kind {
						case search.KindApp:
							ac++
						case search.KindDir:
							dc++
						case search.KindCommand:
							cc++
						default:
							fc++
						}
					}
					log.Printf("  结果 %d: 应用 %d, 文件 %d, 目录 %d, 命令 %d", len(res), ac, fc, dc, cc)
					if len(res) == 0 {
						log.Print("  （没有结果）")
					}
				}
			}
		})
	}()
}

// historyResults 把历史记录还原成可展示的条目（空查询时）。
// 优先用记录里的元数据；老记录（只有 path）才回退到按路径猜。
func (a *application) historyResults(n int) []search.Result {
	// 首页顺序 = 「已固定」在前（按最近使用），后面接「最近使用」（去掉已固定的）。
	pinned := a.hist.Pinned(maxPinned)
	pinnedOnly := make(map[string]bool, len(pinned))
	for _, e := range pinned {
		pinnedOnly[e.Path] = true
	}
	// 顺序是「最近使用」在前、「已固定」在后（Pane 的首页就是这样：常用的贴手，
	// 固定的沉底）。索引与显示顺序保持一致，方向键按行移动才不会跳段。
	entries := make([]history.Entry, 0, len(pinned)+n)
	for _, e := range a.hist.Recent(0) {
		if pinnedOnly[e.Path] {
			continue
		}
		entries = append(entries, e)
		if len(entries) >= n {
			break
		}
	}
	entries = append(entries, pinned...)
	a.pinnedCount = len(pinned)
	out := make([]search.Result, 0, len(entries))
	if debugKeys {
		log.Printf("history: %d 条（其中固定 %d）", len(entries), len(pinned))
	}
	for _, e := range entries {
		name, kind, ext, hint := e.Name, kindOf(e.Kind), e.Ext, e.Hint
		if name == "" {
			name = filepath.Base(e.Path)
		}
		if hint == "" {
			if kind == search.KindApp {
				hint = "应用"
			} else if kind == search.KindCommand {
				hint = "命令：" + e.Exec
			} else {
				hint = filepath.Dir(e.Path)
			}
		}
		if ext == "" && kind == search.KindFile {
			ext = index.Ext(name)
		}
		item := search.Item{
			Name: name,
			Path: e.Path,
			Ext:  ext,
			Kind: kind,
			Hint: hint,
		}
		if kind == search.KindCommand {
			item.Command = e.Exec
			item.Args = e.Args
		}
		out = append(out, search.Result{
			Item:      item,
			Frequency: a.hist.Score(e.Path),
			Accuracy:  1,
			TypeScore: search.TypeScore(ext, kind, a.cfg.Search.TypeOrder),
		})
	}
	return out
}

func kindOf(k history.Kind) search.Kind {
	switch k {
	case history.KindApp:
		return search.KindApp
	case history.KindDir:
		return search.KindDir
	case history.KindCommand:
		return search.KindCommand
	}
	return search.KindFile
}

// searchAll 把命令、已安装应用、索引条目放在一起打分排序。
func (a *application) searchAll(q string) []search.Result {
	// 正则模式是另一条路：不拼音、不模糊、不打分档。这里一分流，下面所有
	// "token / 子序列 / 拼音" 的逻辑对它就完全不参与。
	if a.regexOn {
		return a.searchRegex(q)
	}
	// 通配符也走"模式"那条路：`*.pdf` 里的 `*` 在模糊模式里是普通字符，
	// 子串预筛会要求文件名里真有一个星号，结果一条候选都进不来（用户看到的就是
	// "搜不到"）。见 search.HasGlob 的口径：只有 `*`/`?` 才改道。
	if search.HasGlob(q) {
		return a.searchGlob(q)
	}
	lowQ := strings.ToLower(q)
	tokens := strings.Fields(lowQ)
	weights := search.Weights(a.cfg.Search.Weights)

	results := make([]search.Result, 0, 256)

	// 1) 自定义命令：激活词（主词或别名）整词命中优先（Listary 式）
	for _, cmd := range a.cfg.Commands {
		words, ok := commandWords(cmd, q)
		if !ok {
			continue
		}
		dir := a.lastFollowDir()
		args := expandArgs(cmd.Args, words, dir)
		results = append(results, search.Rank(weights, a.cfg.Search.TypeOrder, search.Result{
			Item: search.Item{
				Name:     cmd.Name,
				Path:     cmd.Exec,
				Hint:     "命令：" + cmd.Exec + " " + strings.Join(args, " "),
				Kind:     search.KindCommand,
				Command:  cmd.Exec,
				Args:     args,
				Dir:      expandTemplate(cmd.Dir, words, dir),
				KeepOpen: cmd.KeepOpen,
			},
			// 命令不是文件，没有"打开历史"，给一个固定的存在感；
			// 命令之间本来就在同一段里，这项只影响命令内部的先后。
			Frequency: 0.5,
			Accuracy:  1,
		}))
	}

	// 2) 已安装应用：名字（含拼音）一路，产品名一路，取分高的那个。
	// 不把 Path 交给匹配器——理由见 search.AppAlias。
	if apps := a.apps.Load(); apps != nil {
		for i, app := range *apps {
			acc, ok := search.AccuracyPy(q, app.Name, "", app.initials, app.full)
			if app.alias != "" {
				if alt, ok2 := search.AccuracyAlias(q, app.alias); ok2 && alt > acc {
					acc, ok = alt, true
				}
			}
			if !ok {
				continue
			}
			freq, opens := a.hist.ScoreCount(app.Path)
			results = append(results, search.Rank(weights, a.cfg.Search.TypeOrder, search.Result{
				Item: search.Item{
					Name:     app.Name,
					Path:     app.Path,
					Ext:      ".exe",
					Kind:     search.KindApp,
					Hint:     "应用",
					AppIndex: i,
				},
				Frequency: freq,
				Opens:     opens,
				Accuracy:  acc,
			}))
		}
	}

	// 3) 索引条目
	tab := a.cat.Load()
	if tab == nil || tab.Len() == 0 || len(tokens) == 0 {
		return a.finish(results)
	}
	byPath := strings.ContainsAny(q, `\/`)
	matched := 0
	for i := 0; i < tab.Len(); i++ {
		var path string
		if byPath {
			path = tab.PathOf(int32(i))
			if !strings.Contains(strings.ToLower(path), lowQ) {
				continue
			}
		} else if !tab.Hits(i, tokens) && !pyContains(tab.PyOf(i), tokens) {
			// 拼音查询（dbgz）在名字里当然找不到字面，得靠预存的拼音首字母串放行
			continue
		} else if tab.Dead(int32(i)) {
			// 墓碑/被删目录的子条目。放在预筛之后才划算：全表上溯一遍要多花
			// 几十毫秒，而这里每条查询只有几十个候选。
			// （按路径查的那条分支走 PathOf，它自带同样的判定）
			continue
		}
		name := tab.Name(i)
		acc, ok := search.AccuracyPy(q, name, path, tab.PyOf(i), "")
		if !ok {
			continue
		}
		if path == "" {
			path = tab.PathOf(int32(i))
		}
		freq, opens := a.hist.ScoreCount(path)
		ext := tab.Ext(i)
		results = append(results, search.Rank(weights, a.cfg.Search.TypeOrder, search.Result{
			Item: search.Item{
				Name: name,
				Path: path,
				Ext:  ext,
				Kind: toSearchKind(tab.KindOf(int32(i))),
				Hint: tab.Dir(int32(i)),
			},
			Frequency: freq,
			Opens:     opens,
			Accuracy:  acc,
		}))
		matched++
		if matched >= 5000 { // 上限保护：极端宽泛查询不至于卡死
			break
		}
	}
	return a.finish(results)
}

// maxRegexHits 是正则模式收集结果的上限。模糊搜索那边是 5000（子串预筛本身就很窄），
// 正则命中面更宽（`\.log$` 就是八千多条），收得太少会在排序之前就把该在前面的丢掉。
const maxRegexHits = 20000

// searchRegex 是正则模式的搜索：把整串查询当 Go 正则，只做**精确匹配**。
// 不叠拼音、不叠模糊子序列、不做准确度分档 —— 用户写了模式，命中与否他说了算，
// 准确度一律 1.0，排序只剩频次与类型这两项。
//
// 只匹配**文件名**。普通模式靠"查询里有没有 \ 或 /"来决定要不要匹配路径，
// 正则里的反斜杠到处都是（`\.log$`），那个判断在这里不成立；想按路径找就自己写
// 目录部分的模式。代价是路径匹配（PathOf 顺父链还原，很贵）在这条路上完全不发生。
func (a *application) searchRegex(q string) []search.Result {
	return a.searchPattern(q, false)
}

// searchGlob 是通配符模式的搜索：`*.pdf` 这类输入。翻译成正则后与正则模式
// **共用同一条路径**（同一套预筛、同一套收集上限、同样按频次与类型排序），
// 差别只在"模式从哪来"。见 internal/search/glob.go。
func (a *application) searchGlob(q string) []search.Result {
	return a.searchPattern(q, true)
}

// searchPattern 是两条"模式型查询"共用的一段：编译模式 → 命令/应用/索引逐个 Match。
// glob 为 true 时编译通配符，否则编译 Go 正则。
func (a *application) searchPattern(q string, glob bool) []search.Result {
	var (
		rx  *search.Regex
		err error
	)
	if glob {
		rx, err = search.CompileGlob(q)
	} else {
		rx, err = search.CompileRegex(q)
	}
	if err != nil {
		// 语法错误必须显式说：否则用户看到空结果会以为"模式没生效"。
		a.setPatternErr(q, err.Error())
		return nil
	}
	a.setPatternErr(q, "")

	weights := search.Weights(a.cfg.Search.Weights)
	acc := search.RegexAccuracy
	results := make([]search.Result, 0, 256)

	// 1) 自定义命令：按名字匹配。普通模式那套"激活词"在正则里没有意义。
	for _, cmd := range a.cfg.Commands {
		name := []byte(cmd.Name)
		if !rx.Maybe(name) || !rx.Match(name) {
			continue
		}
		dir := a.lastFollowDir()
		results = append(results, search.Rank(weights, a.cfg.Search.TypeOrder, search.Result{
			Item: search.Item{
				Name:     cmd.Name,
				Path:     cmd.Exec,
				Hint:     "命令：" + cmd.Exec,
				Kind:     search.KindCommand,
				Command:  cmd.Exec,
				Args:     expandArgs(cmd.Args, nil, dir),
				Dir:      expandTemplate(cmd.Dir, nil, dir),
				KeepOpen: cmd.KeepOpen,
			},
			Frequency: 0.5,
			Accuracy:  acc,
		}))
	}

	// 2) 已安装应用：只认名字。拼音首字母 / 全拼在这条路上一次都不查。
	if apps := a.apps.Load(); apps != nil {
		for i, app := range *apps {
			name := []byte(app.Name)
			if !rx.Maybe(name) || !rx.Match(name) {
				continue
			}
			freq, opens := a.hist.ScoreCount(app.Path)
			results = append(results, search.Rank(weights, a.cfg.Search.TypeOrder, search.Result{
				Item: search.Item{
					Name:     app.Name,
					Path:     app.Path,
					Ext:      ".exe",
					Kind:     search.KindApp,
					Hint:     "应用",
					AppIndex: i,
				},
				Frequency: freq,
				Opens:     opens,
				Accuracy:  acc,
			}))
		}
	}

	// 3) 索引条目
	tab := a.cat.Load()
	if tab == nil || tab.Len() == 0 {
		return a.finish(results)
	}
	matched := 0
	for i := 0; i < tab.Len(); i++ {
		// 全量扫一条不筛的正则要几百毫秒，用户早换关键字了就别把这遍跑完：
		// refilter 每敲一个键都会起一个 goroutine，旧的不退出就会互相抢 CPU。
		if i&2047 == 0 && strings.TrimSpace(a.query) != q {
			return nil
		}
		name := tab.NameBytes(i)
		if !rx.Maybe(name) || !rx.Match(name) {
			continue
		}
		// 命中之后再判墓碑：正则能命中几千条，上溯这几千次约 1ms，
		// 放在预筛之前就对 179 万条都做了。
		if tab.Dead(int32(i)) {
			continue
		}
		path := tab.PathOf(int32(i))
		ext := tab.Ext(i)
		freq, opens := a.hist.ScoreCount(path)
		results = append(results, search.Rank(weights, a.cfg.Search.TypeOrder, search.Result{
			Item: search.Item{
				Name: tab.Name(i),
				Path: path,
				Ext:  ext,
				Kind: toSearchKind(tab.KindOf(int32(i))),
				Hint: tab.Dir(int32(i)),
			},
			Frequency: freq,
			Opens:     opens,
			Accuracy:  acc,
		}))
		matched++
		if matched >= maxRegexHits {
			break
		}
	}
	return a.finish(results)
}

// setPatternErr 记住（或清空）上一次"模式型查询"的编译错误。
// 和 refilter 一样校验查询没变：搜索在后台 goroutine 里跑，回来时用户可能已经改了输入。
func (a *application) setPatternErr(q, msg string) {
	if a.panel == nil {
		a.patternErr = msg
		return
	}
	a.panel.Update(func() {
		if strings.TrimSpace(a.query) == q {
			a.patternErr = msg
		}
	})
}

func (a *application) truncate(results []search.Result) []search.Result {
	max := a.cfg.Search.MaxResults
	if max > 0 && len(results) > max {
		return results[:max]
	}
	return results
}

// finish 是搜索的最后一步：排序 → 按配额截到 MaxResults。
//
// 这里不再调用 search.Balance（打散应用的连续块）。类型现在是硬分段，
// 再交错等于把文件硬塞回应用前面，正好和"输 qq 先看到 QQ"相反；
// "输入 d 只显示应用"由 truncateMixed 的保底名额兜住——80 条里始终留 20 条给非应用。
func (a *application) finish(results []search.Result) []search.Result {
	search.Sort(results)
	return a.truncateMixed(results)
}

// truncateMixed 按配额截断：应用最多占 (max - 非应用保底) 条，剩下的名额强制留给文件/目录。
// max<=0 或结果本来就不超限时原样返回。
func (a *application) truncateMixed(results []search.Result) []search.Result {
	max := a.cfg.Search.MaxResults
	if max <= 0 || len(results) <= max {
		return results
	}
	var apps, others []search.Result
	for _, r := range results {
		if r.Kind == search.KindApp {
			apps = append(apps, r)
		} else {
			others = append(others, r)
		}
	}
	if len(others) == 0 {
		return results[:max]
	}
	reserve := max / 4 // 默认 80 条里留出 20 条给非应用
	if reserve > len(others) {
		reserve = len(others)
	}
	appQuota := max - reserve
	if appQuota > len(apps) {
		appQuota = len(apps)
	}
	out := make([]search.Result, 0, max)
	out = append(out, apps[:appQuota]...)
	othersQuota := max - appQuota
	if othersQuota > len(others) {
		othersQuota = len(others)
	}
	out = append(out, others[:othersQuota]...)
	search.Sort(out)
	return out
}

func toSearchKind(k index.Kind) search.Kind {
	if k == index.KindDir {
		return search.KindDir
	}
	return search.KindFile
}

// pyContains 是拼音预筛：名字的拼音首字母串里包含全部 token 才放行。
// 只做"包含"不做散落子序列——后者在百万条目上太贵，收益也低。
// 只做"包含"不做散落子序列——后者在百万条目上太贵，收益也低。
func pyContains(py string, tokens []string) bool {
	if py == "" {
		return false
	}
	for _, t := range tokens {
		if !isAlphaNum(t) || !strings.Contains(py, t) {
			return false
		}
	}
	return true
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func (a *application) choose(i int) {
	a.chosen = max(0, min(i, len(a.results)-1))
	if len(a.results) > 0 {
		a.list.ScrollIntoView(a.chosen)
	}
}

// keyStep 是首页的方向键：按下的那一下把选中项挪 step 格。
// 用按住状态做边沿检测，而不是 Shortcut——焦点在搜索框里时左右键会被输入框吃掉。
func (a *application) keyStep(c *ui.Context, held *bool, key ui.Key, step int) {
	now := c.KeyDown(key)
	if now && !*held {
		a.choose(a.chosen + step)
	}
	*held = now
}

// open 执行选中的结果并隐藏面板。
func (a *application) open() {
	if a.chosen < 0 || a.chosen >= len(a.results) {
		return
	}
	r := a.results[a.chosen]
	entry := entryOf(r)
	if a.offline {
		// 视图测试（ui.Tester 没有 native 主循环）：Shell.OpenPath / LaunchApp 会永久阻塞，
		// 这里只记一笔历史，让"点了有没有反应"可被断言。
		a.hist.TouchEntry(entry)
		return
	}
	// 命令可以要求"执行后别收面板"（KeepOpen）：面板先收再跑，是默认行为。
	if !(r.Kind == search.KindCommand && r.KeepOpen) {
		a.hidePanel()
	}

	// 历史要带着"这是什么"一起记，否则重启后还原不出正确的名字和图标。
	log.Printf("open: %s (%s)", r.Name, r.Path)

	if r.Kind == search.KindCommand {
		prog, args := launchSpec(r.Command, r.Args)
		cmd := exec.Command(prog, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
		if r.Dir != "" {
			cmd.Dir = r.Dir
		}
		if err := cmd.Start(); err != nil {
			log.Printf("运行命令 %s %v（工作目录 %q）: %v", prog, args, r.Dir, err)
			return
		}
		a.hist.TouchEntry(entry)
		return
	}

	a.hist.TouchEntry(entry)
	if r.Kind == search.KindApp {
		if apps := a.apps.Load(); apps != nil {
			for _, app := range *apps {
				if app.Path == r.Path || app.ID == r.Path {
					if err := mygo.Shell.LaunchApp(app.InstalledApp); err != nil {
						log.Printf("启动 %s: %v", r.Name, err)
					}
					return
				}
			}
		}
	}
	// 打开也走 iconPath：应用清单里找不到对应条目时，这条 Path 可能是
	// "shell:AppsFolder\<真路径>" 的畸形写法，交给 Shell 解析会打不开，
	// 剥成普通路径反而能正常启动。正常 AUMID 不受影响（iconPath 原样返回）。
	if err := mygo.Shell.OpenPath(iconPath(r.Path)); err != nil {
		log.Printf("打开 %s: %v", r.Path, err)
	}
}

// togglePin 把选中项固定到首页 / 取消固定（Pane 的 Ctrl+K）。
// 固定状态存在 history.Entry 上，随 history.json 一起落盘。
func (a *application) togglePin() {
	if a.chosen < 0 || a.chosen >= len(a.results) {
		return
	}
	r := a.results[a.chosen]
	key := r.Path
	if key == "" {
		key = r.Command
	}
	if key == "" {
		return
	}
	if a.hist.Count(key) == 0 {
		// 没打开过的项在历史里还不存在，先记一条再去固定（否则无处存标记）。
		e := entryOf(r)
		if e.Path == "" {
			e.Path = key
		}
		a.hist.TouchEntry(e)
	}
	pinned, ok := a.hist.TogglePin(key)
	if !ok {
		return
	}
	log.Printf("pin: %s -> %v", r.Name, pinned)
	if pinned {
		a.setStatus("已固定到首页：" + r.Name)
	} else {
		a.setStatus("已取消固定：" + r.Name)
	}
	// 首页要把固定项挪进「已固定」区，所以重建结果，并把选中落回原来那一项。
	if a.query == "" {
		a.refilter()
		for i := range a.results {
			if a.results[i].Path == r.Path && a.results[i].Name == r.Name {
				a.chosen = i
				break
			}
		}
	}
}

// openFolder 打开选中项所在的文件夹（Ctrl+Enter）。
func (a *application) openFolder() {
	a.openFolderAt(a.chosen)
}

// openFolderAt 打开第 i 条结果所在的文件夹（文件开父目录，目录开自身）。
// 键盘（Ctrl+Enter）走选中项，右键菜单走被点的那一条，所以带 index。
func (a *application) openFolderAt(i int) {
	if i < 0 || i >= len(a.results) {
		return
	}
	r := a.results[i]
	if !hasContainingFolder(r) {
		a.setStatus("这一项没有所在文件夹")
		return
	}
	target := r.Path
	if r.Kind != search.KindDir {
		target = filepath.Dir(r.Path)
	}
	if err := mygo.Shell.OpenPath(target); err != nil {
		a.setStatus("打开文件夹失败：" + err.Error())
		return
	}
	a.hidePanel()
}

// copyDir 把选中项所在目录放进剪贴板。对话框跟随是自动的（见 win.DialogFollow），
// 这条留作后备：对话框被别的程序挡住、或那套 WM_SETTEXT 被外壳拒掉时还能手工粘一次
// （框里 Ctrl+L → Ctrl+V → 回车）。
func (a *application) copyDir(c *ui.Context) {
	if a.offline {
		return
	}
	if a.chosen < 0 || a.chosen >= len(a.results) {
		a.setStatus("没有选中项")
		return
	}
	dir := dirOf(a.results[a.chosen])
	if dir == "" {
		a.setStatus("这一项没有路径")
		return
	}
	c.WriteClipboardThen(dir, func(err error) {
		if err != nil {
			a.setStatus("复制失败：" + err.Error())
			return
		}
		a.setStatus("已复制目录：" + dir)
	})
}

// dirOf 是一条结果对应的目录：目录给自身，文件给所在目录，虚拟项（shell:）没有目录。
func dirOf(r search.Result) string {
	if r.Path == "" || strings.HasPrefix(r.Path, "shell:") {
		return ""
	}
	if r.Kind == search.KindDir {
		return r.Path
	}
	return filepath.Dir(r.Path)
}

// entryOf 把一条搜索结果转成历史条目：保留名字、类型、扩展名与提示。
func entryOf(r search.Result) history.Entry {
	return history.Entry{
		Path: r.Path,
		Name: r.Name,
		Kind: historyKind(r.Kind),
		Ext:  r.Ext,
		Hint: r.Hint,
		Exec: r.Command,
		Args: r.Args,
	}
}

func historyKind(k search.Kind) history.Kind {
	switch k {
	case search.KindApp:
		return history.KindApp
	case search.KindDir:
		return history.KindDir
	case search.KindCommand:
		return history.KindCommand
	}
	return history.KindFile
}

// panelSettle 是面板弹出后允许位置再被系统/mygo 改动的窗口期。这段时间里的移动
// 不是用户拖的，不记进配置；过了这段时间，位置才算数。
const panelSettle = 900 * time.Millisecond

// placePanel 把面板摆到 (x, y)：先摆一次，弹出过程结束后再补一次。
// 补的那次是关键——面板刚显示时位置会被改掉（见 panelPlaceAt 的注释）。
func (a *application) placePanel(x, y int) {
	a.panelMu.Lock()
	a.panelPlaceAt = time.Now()
	a.panelMu.Unlock()
	a.panel.SetPosition(x, y)
	time.AfterFunc(panelSettle/2, func() { a.panel.SetPosition(x, y) })
}

// hidePanel 收起面板时连阴影层一起收：阴影是另一个独立窗口，面板藏起来它不会
// 自己消失（实测会孤零零留在桌面上，还会被下一次唤出的桌面取色采进去）。
func (a *application) hidePanel() {
	a.panelUp.Store(false)
	a.hideShadow()
	a.panel.Hide()
	// 半分辨率整屏快照只在"面板开着"这段时间有用（四角弧外要按当前位置裁），
	// 面板收起就放掉，3.7 MB 不占着常驻。
	//
	// 磨砂底片**故意留着**（230KB 的缩略图）：它是"最后一次没有任何自家窗口在屏上
	// 时抓到的桌面"。子窗（设置/互传）开着的时候再抓屏只可能抓到自家窗口 ——
	// 见 glass.go 的 snapshotScreenExcept；那种时候就靠这张旧底片顶上去，
	// 几何上它照样对得上屏幕，只是桌面内容旧一点。
	a.deskHalf = nil
}

// finishResize 是缩放停手后的一次收尾：重设圆角区域、重画四角月牙，最后把阴影
// 层按新的尺寸重建并显示出来。
//
// 顺序不能反：阴影位图是按最终尺寸算的，得等尺寸定了再算；四角月牙要在阴影显示
// 之前重画，因为月牙里那块"窗外桌面色"是从面板外侧实时取的，阴影已经亮起来的话
// 会把那几个取样点压暗。
func (a *application) finishResize() {
	// 收尾走主线程：buildCornerBitmaps 要造 ui.Bitmap（GPU 纹理），而
	// time.AfterFunc 的回调跑在普通 goroutine 上 —— 和渲染线程同时碰 D3D
	// 设备是没保证的。RunOnMain 会在主线程上等它跑完（本来就在主线程时直接跑）。
	start := time.Now()
	var dRegion, dCorners, dShadow, dBack time.Duration
	mygo.RunOnMain(func() {
		t := time.Now()
		a.applyRoundRegion()
		dRegion = time.Since(t)

		t = time.Now()
		// force=false：尺寸/位置里和四角位图无关的那部分变了也照旧复用旧图。
		a.refreshCorners(false)
		dCorners = time.Since(t)

		t = time.Now()
		// 尺寸定了，底图按最终客户区重烘一张（弧和边框都跟着新尺寸走）。
		a.refreshBacking(true)
		dBack = time.Since(t)

		t = time.Now()
		a.refreshShadow()
		dShadow = time.Since(t)
	})
	// 松手那一下如果有卡顿，这行把责任分清楚：是区域、四角、阴影，还是这三样
	// 之外（那就是 mygo 自己按新尺寸重画的那一帧）。
	if d := time.Since(start); d > 10*time.Millisecond {
		log.Printf("缩放: 收尾 %.2fms（区域 %.2f + 四角 %.2f + 底图 %.2f + 阴影 %.2f）",
			float64(d.Microseconds())/1000,
			float64(dRegion.Microseconds())/1000,
			float64(dCorners.Microseconds())/1000,
			float64(dBack.Microseconds())/1000,
			float64(dShadow.Microseconds())/1000)
	}
	// 一轮拖动的数据留一行日志：帧数、回调耗时、帧间隔峰值。
	// 帧数远小于鼠标移动次数（比如 400 次移动只来了 15 帧）就说明事件是被系统
	// 或面板绘制并掉的，不是我们的回调慢；反过来回调 max 高就得先抠这里。
	a.panelMu.Lock()
	n, sum, max, gap := a.resizeFrames, a.resizeSum, a.resizeMax, a.resizeGapMax
	span := time.Since(a.resizeStartedAt)
	a.resizeFrames, a.resizeSum, a.resizeMax, a.resizeGapMax = 0, 0, 0, 0
	a.resizePrev = time.Time{}
	a.panelMu.Unlock()
	if n > 0 {
		avg := float64(sum.Microseconds()) / 1000 / float64(n)
		log.Printf("缩放: 本轮 %d 帧，跨度 %.0fms，回调 avg %.2fms max %.2fms，帧间隔 max %.0fms",
			n, float64(span.Microseconds())/1000, avg,
			float64(max.Microseconds())/1000, float64(gap.Microseconds())/1000)
	}
}

// panelGeometryChanged 是窗口尺寸/位置变化的回调：攒到用户停手再落盘。
// 拖动时这个回调会连着来几十次（每帧一次），每次都写 config.json 太重。
// 尺寸和位置走同一个定时器：两件事都要写同一个文件，分开定定时器会互相覆盖。
func (a *application) panelGeometryChanged() {
	w, h := a.panel.Size()
	x, y := a.panel.Position()
	a.panelMu.Lock()
	if time.Since(a.panelPlaceAt) < panelSettle {
		// 面板正在弹出，这期间的位移是显示过程造成的，**不记**——
		// 否则每次唤出都把记录的位置改写成"被挪到的那个值"，复现就永远对不上。
		a.panelMu.Unlock()
		return
	}
	a.panelMu.Unlock()
	if !a.panel.IsVisible() {
		// 面板收起来之后它还会被系统挪一下（实测：隐藏几秒后位置会漂走）。
		// 那不是用户拖的，记进去就等于每唤出一次把位置改一次。
		log.Printf("面板几何: 隐藏状态下的移动 %d,%d，忽略", x, y)
		return
	}
	a.panelMu.Lock()
	a.panelPendW, a.panelPendH = w, h
	a.panelPendX, a.panelPendY = x, y
	if a.panelSave != nil {
		a.panelSave.Stop()
	}
	a.panelSave = time.AfterFunc(800*time.Millisecond, a.savePanelGeometry)
	a.panelMu.Unlock()
}

// savePanelGeometry 把拖出来的尺寸和位置写进配置。失败只记日志、不改内存里的 cfg，
// 否则会出现"配置文件没写成、但这次运行按新尺寸走"的错位。
func (a *application) savePanelGeometry() {
	a.panelMu.Lock()
	w, h := a.panelPendW, a.panelPendH
	x, y := a.panelPendX, a.panelPendY
	a.panelMu.Unlock()
	if w <= 0 || h <= 0 {
		return
	}
	// 拖到屏幕边缘时面板可以有一截探出工作区；下次启动本来就会被夹回来，
	// 这里先夹一次，省得配置里记的值和实际摆的位置差几像素、每启动一次对不齐。
	near := mygo.Screen.DisplayNearestPoint(mygo.Point{X: x + w/2, Y: y + h/2})
	x, y = clampPanelTo(x, y, w, h, near.WorkArea)
	// 每次唤出面板都会先 SetPosition（还会 SetSize），那也会触发 OnMove/OnResize；
	// 没真变就别重写 config.json——不然每唤出一次就多一次磁盘写。
	cw, ch := a.cfg.PanelSize()
	cx, cy, has := a.cfg.PanelPos()
	if w == cw && h == ch && has && x == cx && y == cy {
		return
	}
	cfg := *a.cfg
	cfg.Panel.Width, cfg.Panel.Height = w, h
	px, py := x, y
	cfg.Panel.X, cfg.Panel.Y = &px, &py
	if err := config.Save(&cfg, a.cfgPath); err != nil {
		log.Printf("保存面板位置/尺寸: %v", err)
		return
	}
	a.cfg = &cfg
	log.Printf("面板几何: %dx%d @ %d,%d（已保存）", w, h, x, y)
}

// panelPosition 返回面板该摆在哪：拖过就按记下的位置复现，没拖过就居中偏上。
//
// 记下的坐标必须先夹回可见区域：拔掉一块显示器、改了分辨率之后，旧坐标可能
// 整块落在屏外——面板一唤出就在看不见的地方，等于启动器失效了。
func (a *application) panelPosition(d mygo.Display, w, h int) (int, int) {
	if px, py, ok := a.cfg.PanelPos(); ok {
		// 用面板中心去找"最近的显示器"，这样多半屏已经在另一块屏上时也认得对。
		near := mygo.Screen.DisplayNearestPoint(mygo.Point{X: px + w/2, Y: py + h/2})
		return clampPanelTo(px, py, w, h, near.WorkArea)
	}
	return d.WorkArea.X + (d.WorkArea.Width-w)/2, d.WorkArea.Y + (d.WorkArea.Height-h)/3
}

// clampPanelTo 把面板左上角夹进给定工作区，保证它一定看得见。
//
// 面板比工作区还宽/高时（竖屏 + 很宽的面板、4K 拖到 1080p 屏上），
// "右边界 - 宽"会算出小于原点的负偏移，所以外面还要 max 一次：宁可右边溢出，
// 也要让左上角（顶栏那条搜索框）留在可见区域。
func clampPanelTo(x, y, w, h int, wa mygo.Rectangle) (int, int) {
	return max(wa.X, min(x, wa.X+wa.Width-w)), max(wa.Y, min(y, wa.Y+wa.Height-h))
}

// show 在鼠标所在显示器居中偏上位置显示面板。
// startWatchdog 盯着 UI 线程。拖动期间它每 200ms 检查一次心跳（OnMove/OnResize
// 会更新 uiTick）；超过 1.2 秒没有心跳就把所有 goroutine 的栈打进日志。
//
// 之所以要这个：用户报的"快速拖动卡死"在合成拖动里复现不出来（实测 750 次/秒
// 的移动，回调 0.5~1ms），那就得让真机上的下一次卡顿自己把现场留下来 ——
// 栈里能看出是阻塞在 WaitMessage、D3D 提交，还是我们自己的锁上。
// startMenuRoots 是开始菜单的两个目录：全机的与当前用户的。
func startMenuRoots() []string {
	var roots []string
	if d := os.Getenv("ProgramData"); d != "" {
		roots = append(roots, filepath.Join(d, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	if d := os.Getenv("APPDATA"); d != "" {
		roots = append(roots, filepath.Join(d, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	return roots
}

// startMenuModTime 返回开始菜单目录树里最新的 mtime，没有目录时为零值。
// 新装的软件会往开始菜单里放 .lnk（常在子目录里），所以要看整棵树，
// 只看两个根目录不够——子目录的 mtime 不会冒到父目录。
func startMenuModTime() time.Time {
	var newest time.Time
	for _, root := range startMenuRoots() {
		_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
	}
	return newest
}

// loadApps 读开始菜单，换上新列表并记住这次看到的 mtime。
// initial 为 false 时，读完若面板正开着就重排一次结果——新装的应用立刻能搜到。
func (a *application) loadApps(initial bool) {
	defer a.appsReloading.Store(false)
	raw, err := mygo.Shell.InstalledApps()
	if err != nil {
		log.Printf("读取已安装应用: %v", err)
		return
	}
	entries := make([]appEntry, 0, len(raw))
	for _, app := range raw {
		entries = append(entries, appEntry{
			InstalledApp: app,
			initials:     pinyin.Initials(app.Name),
			full:         pinyin.Full(app.Name),
			alias:        search.AppAlias(app.ID),
		})
	}
	a.apps.Store(&entries)
	a.appsMTime.Store(startMenuModTime().UnixNano())
	if debugKeys {
		log.Printf("应用: %d 个", len(entries))
	}
	if initial {
		return
	}
	log.Printf("应用列表已更新: %d 个", len(entries))
	mygo.RunOnMain(func() {
		if a.quitting.Load() || a.panel == nil || !a.panelUp.Load() {
			return
		}
		a.refilter()
		a.panel.Update(func() {})
	})
}

// reloadAppsIfStale 是唤出时的轻量检查：开始菜单的 mtime 比上次读列表时新，
// 就在后台重读一次。检查递归走一遍开始菜单，几毫秒；两秒内重复唤出不再查。
func (a *application) reloadAppsIfStale() {
	now := time.Now()
	if now.Sub(a.appsChecked) < 2*time.Second {
		return
	}
	a.appsChecked = now
	mt := startMenuModTime()
	if mt.IsZero() || mt.UnixNano() <= a.appsMTime.Load() {
		return
	}
	if !a.appsReloading.CompareAndSwap(false, true) {
		return
	}
	log.Print("开始菜单有变化，后台重读应用列表")
	go a.loadApps(false)
}

func (a *application) startWatchdog() {
	go func() {
		// 顺带当外观的定期复查：面板可见时每 5 拍（约 1 秒）验一次"圆角区域还在、
		// 阴影层上屏了"，缺了就补。检查本身是两次廉价的 Win32 查询，不占 CPU；
		// 之所以敢每秒查，是因为"偶尔变直角"这种毛病没法靠一次性的时序修复兜住。
		beats := 0
		for {
			time.Sleep(200 * time.Millisecond)
			// 面板开着的判断用 panelUp（见它的注释），不用系统的 IsWindowVisible。
			if a.panel == nil || !a.panelUp.Load() {
				a.hangDumped.Store(false)
				beats = 0
				continue
			}
			beats++
			if beats%5 == 0 && !a.dragging() {
				mygo.RunOnMain(func() { a.ensureShell("空闲复查") })
			}
			until := a.dragUntil.Load()
			tick := a.uiTick.Load()
			if until == 0 || time.Now().UnixNano() > until {
				a.hangDumped.Store(false)
				continue
			}
			idle := time.Since(time.Unix(0, tick))
			if idle < 1200*time.Millisecond {
				a.hangDumped.Store(false)
				continue
			}
			if !a.hangDumped.CompareAndSwap(false, true) {
				continue
			}
			var buf bytes.Buffer
			_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
			log.Printf("看门狗: 拖动中 UI 线程 %dms 没有心跳，goroutine 栈：\n%s",
				int(idle.Milliseconds()), buf.String())
		}
	}()
}

func (a *application) show() {
	if a.cfg.Hotkey.DisableInFullscreen && win.ForegroundInfo().Fullscreen {
		log.Print("前台是全屏应用，忽略唤出")
		return
	}
	// 先把这个开关立起来：后面的定期外观复查（ensureShell）以它为准。
	// 唤出耗时：一段一段量，最后打成一行。低配机上"呼出有点卡"没法靠猜 ——
	// 得先知道这几十毫秒花在抓屏、底图、首帧哪一段上。
	prev := time.Now()
	spent := make([]string, 0, 12)
	lap := func(name string) {
		d := time.Since(prev)
		spent = append(spent, fmt.Sprintf("%s %.1fms", name, float64(d.Microseconds())/1000))
		prev = time.Now()
	}
	defer func() {
		log.Print("唤出耗时: ", strings.Join(spent, " / "))
	}()
	a.panelUp.Store(true)
	a.shellProbes = 0
	// 上一次收起时阴影层可能还亮着（面板藏了，阴影是另一个窗口，不会自己消失），
	// 这里先收掉：下面的桌面底色取样是从面板外侧实时取的，自己的阴影会把那几
	// 个点压暗（实测角上取到 #D4D4D4 而不是真桌面）。
	a.hideShadow()
	d := mygo.Screen.DisplayNearestPoint(mygo.Screen.CursorScreenPoint())
	w, h := a.panel.Size()
	x, y := a.panelPosition(d, w, h)
	if px, py, ok := a.cfg.PanelPos(); ok {
		log.Printf("唤出: 记下的位置 %d,%d → 实际 %d,%d（面板 %dx%d，工作区 %+v）", px, py, x, y, w, h, d.WorkArea)
	} else {
		log.Printf("唤出: 没记过位置，居中偏上 → %d,%d（工作区 %+v）", x, y, d.WorkArea)
	}
	a.placePanel(x, y)
	a.query = ""
	// 唤出前把索引追一次：这是"刚存的文件马上能搜到"的关键一步。
	// 没变化时只是两次 stat（微秒级）；有变化才复制快照叠增量（约 15ms），
	// 而这点时间藏在面板动画里，感知不到。
	a.refreshIndex(true)
	lap("索引")
	// 应用列表也顺手查一次：新装的软件会改开始菜单，变了就后台重读，
	// 不用重启 Baozi（文件索引是实时的，应用列表没理由滞后）。
	a.reloadAppsIfStale()
	a.refilter()
	lap("筛选（含应用列表）")
	// 玻璃要在 Show 之前抓：那一刻窗口还没上屏，抓到的才是它后面的画面。
	a.refreshGlass()
	lap("抓屏+磨砂")
	// 圆角区域要在 Show **之前**就设好。
	//
	// 这个窗口是 noRedirect 合成的：第一帧如果按矩形合成，四角就是四块半透明的
	// 直角；而 SetWindowRgn 的 bRedraw=0（为了缩放跟手，见 region.go）不会让窗口
	// 重画，所以那一帧会一直留在屏幕上，直到下一次 Show 把它换掉 —— 用户看到的
	// 就是"偶尔出现四个直角，再激活一次面板又消失了"。
	a.applyRoundRegion()
	a.panel.Show()
	lap("Show")
	// 窗口区域：这块窗口是 noRedirect 合成的，圆角以外的像素透不出桌面、DWM 会
	// 填白，所以形状只能靠区域切。半径比内容圆角大 glassRegionSlack，让那条 1 位
	// 掩码的硬边落在四角月牙铺的纯桌面色里；弧本身的抗锯齿由月牙的 alpha 给。
	// 区域同时决定 DWM 阴影的形状，这也是阴影能跟着圆角走的唯一办法。
	// Show 内部会动窗口样式/尺寸，所以上屏后再设一次（两次都只是 1ms 级的调用）。
	a.applyRoundRegion()
	// DWM 那条 1 像素边框在 Show 之后再关一次：mygo 每次显示窗口都会把窗口样式
	// 动一遍，属性不一定留着。它只在面板是前台活动窗口时才画、只从 alpha=0 的
	// 圆角缺口里透出来 —— 就是四角上那一格白。
	a.disableDwmFrame()
	// 四角的桌面色要在窗口移到本次位置之后再取（取的是窗外 2px 的实时光片，
	// 现在优先从刚刚抓的那张半分辨率快照里读，见 glass.go 的 colorAt）—— 这
	// 件事不能放到 Show 之前，否则读到的还是上一处桌面。
	// 面板四角露出的那块底色（deskColor）由它给出，每一次唤出都要更新。
	a.pickDeskColor(nil)
	lap("桌面色")
	// force=true：每次唤出都重建一次，哪怕参数和上次一模一样（上一轮可能是从
	// 别的窗口状态退下来的）。
	a.refreshCorners(true)
	lap("四角")
	// 底图必须在 Show 之前/之后最早的时刻烘好：面板第一帧铺的就是它，
	// 底图还没生成的那一帧是矩形（用户报的"偶尔出现四个直角"）。
	a.refreshBacking(true)
	lap("底图")
	// 面板的自绘拖动（dragwin.go）：窗口建好、位置定下之后在它的 hwnd 上挂一层
	// 子类化。必须放在 Show 之后 —— mygo 显示窗口时会重设 WndProc，太早挂会被顶掉。
	if hwnd := a.panel.NativeHandle(); hwnd != 0 {
		a.installSelfDrag(hwnd)
	}
	// 唤出即置顶。别的置顶窗口（豆包工作窗就是）盖在上面时，鼠标消息落不到面板，
	// 表现就是"完全拖不动"。
	a.raisePanel()
	lap("置顶")
	// 阴影层放在最后：它是从面板外侧取样的，得等四角的桌面色取完再显示，
	// 否则那几像素的取样会被自己的阴影压暗。
	a.refreshShadow()
	lap("投影")
	// Show 内部只调 SetForegroundWindow，从任务栏/别的窗口切回来时会被前台锁拒绝，
	// 所以这里自己带上 AttachThreadInput 再抢一次；拿不到也不影响面板显示。
	if !forceForeground(a.panel.NativeHandle()) {
		log.Print("唤出：抢前台失败（退化为普通显示）")
	}
	// 焦点要等窗口真的成为前台之后再交给输入框，否则焦点会被重新激活的窗口抢走。
	a.panel.Update(func() { a.input.Focus() })
	lap("首帧")
	// 弹出过程结束后把最终位置记一次：这时它要么是配置里的值，要么是用户已经
	// 拖过的新位置，两种都该落盘。（等于配置时 savePanelGeometry 会跳过写盘。）
	// 顺便在这一刻复查一次外观状态：弹出动画里 mygo 会反复调 SetWindowPos、动窗口
	// 样式，区域偶尔就是在这段时间被清掉的 —— 那正是"四个直角"出现的现场。
	time.AfterFunc(panelSettle+100*time.Millisecond, func() {
		a.panelGeometryChanged()
		mygo.RunOnMain(func() { a.ensureShell("唤出动画结束后") })
	})
	log.Print("toggle: 显示面板")
}

func (a *application) toggle() {
	if a.panel.IsVisible() {
		// 可见但已经不是前台（例如刚点过任务栏）时，这一下应当"取回焦点"，
		// 而不是把面板收起来——否则用户按了快捷键看到面板消失，还得再按一次。
		if a.panel.IsFocused() {
			log.Print("toggle: 隐藏面板")
			a.hidePanel()
			// 收起面板正是回收的好时机：增量每条都复制一份可写表，
			// 那些被换下来的旧表要等 GC，而 GC 不会把页还给 OS。
			releaseMemory()
			return
		}
		log.Print("toggle: 面板可见但未聚焦，取回焦点")
		if !forceForeground(a.panel.NativeHandle()) {
			log.Print("唤出：抢前台失败（退化为普通显示）")
		}
		a.panel.Update(func() { a.input.Focus() })
		return
	}
	a.show()
}

// applyAppearance 把配置里的外观项落到绘制时读的包级变量上：
// 圆角决定底图/内容层弧/阴影层的形状，透明度决定磨砂图上那层白有多浓、糊得多厉害，
// 投影强度决定阴影最深有多黑。
//
// 这些都是绘制时现读的量，所以改完不用重建窗口 —— 重烘一次底图、重做一次磨砂图、
// 重画一次阴影层就够（见 applyAppearanceNow）。唯一没法就地重做的是"抓屏"本身，
// 所以调这些值不会让面板里的桌面内容变成新截取的那一刻。
func applyAppearance(cfg *config.Config) {
	if cfg == nil {
		return
	}
	r := cfg.PanelRadius()
	glassRadius = r
	glassClipRadius = r - 1
	if glassClipRadius < 0 {
		// 直角时内容层不裁（Radius(0) 就是不要圆角）；让 1 DIP 的约定在 0 处失效。
		glassClipRadius = 0
	}
	// 透明度 → 压白 alpha，分两段：0% 是实心 255（完全看不见桌面），59% 是 158
	// （原来那套观感，缺省值，不能变），再往上加陡 —— 100% 压到 62。
	// 早先高段和低段用同一条斜线，100% 只能到 90，用户实际拉满还是觉得"太不透明"；
	// 62 仍留四分之一层白（字压得住、面板有边界），但桌面明显能看清。
	pct := cfg.GlassTransparencyPct()
	alpha := 158
	switch {
	case pct <= config.DefaultGlassTransparency:
		alpha = 255 - int(math.Round(float64(pct)*97.0/float64(config.DefaultGlassTransparency)))
	case pct > config.DefaultGlassTransparency:
		alpha = 158 - int(math.Round(float64(pct-config.DefaultGlassTransparency)*96.0/
			float64(100-config.DefaultGlassTransparency)))
	}
	if alpha < 62 {
		alpha = 62
	}
	if alpha > 255 {
		alpha = 255
	}
	glassTintAlpha = uint8(alpha)
	// 模糊跟着透明度走：越透糊得越轻（想看见桌面就该少糊一点）。
	// 59% 是 9；往上到 100% 降到 5（磨砂图本身已经降采样，5 约合屏幕 20 像素，
	// 桌面只剩明暗和色块，认得是"一片窗外的景色"但认不出图标）；往下到 0% 给到 12。
	blur := 9
	switch {
	case pct <= config.DefaultGlassTransparency:
		blur = 9 + int(math.Round(float64(config.DefaultGlassTransparency-pct)*3.0/59.0))
	case pct > config.DefaultGlassTransparency:
		blur = 9 - int(math.Round(float64(pct-config.DefaultGlassTransparency)*4.0/41.0))
	}
	if blur < 5 {
		blur = 5
	}
	if blur > 12 {
		blur = 12
	}
	glassBlurRadius = blur
	// 投影强度百分比 → 黑度：100% 对应 0.6（再深就显脏，见 internal/win/shadow.go）。
	win.SetShadowAlpha(float64(cfg.ShadowPct()) / 100 * 0.6)
	// 一行把所有折算结果写出来：调外观时"到底生效了没有"不用猜，看日志就行。
	log.Printf("外观: 圆角 %d DIP、透明度 %d%%（压白 alpha %d、模糊 %d）、投影 %d%%",
		r, pct, glassTintAlpha, glassBlurRadius, cfg.ShadowPct())
}

// applyAppearanceNow 让设置页刚改的外观立刻在屏幕上成立：底图的弧/覆盖率按新
// 半径重烘，磨砂图按新透明度/模糊重做一张，内容层那条弧跟着重画，投影层按新强度重画。
//
// 磨砂图能就地重做，是因为唤出时留了一张没动过的桌面底片（a.frostBase）——
// 老实现把它原地糊掉+压白了，所以只能等下次唤出重新抓屏，用户会觉得"点了保存
// 却没反应"。面板藏着时（没有底片）就什么都不用做，下次唤出自然按新设置来。
func (a *application) applyAppearanceNow() {
	// 走主线程：refreshBacking / Bitmap.Update 都要碰 GPU，和渲染线程同时搞 D3D 没保证。
	mygo.RunOnMain(func() {
		// 1) 先把磨砂图按新的透明度/模糊重做一张。这一步必须排在重烘之前：底图
		//    里每个玻璃像素都是拿它采样来的，先重烘再换磨砂图，重烘出来的还是旧图。
		//    磨砂图能就地重做，是因为唤出时留了一张没动过的桌面底片（a.frostBase）。
		a.rebuildFrost()
		// 2) 面板区域（bakedPanel 下是空操作）与面板自己的底图、投影。
		//    面板藏着时**不要**碰它：refreshShadow 会按窗口矩形重摆投影层，
		//    而隐藏窗口的矩形是旧位置，摆出来就是桌上一个没人认领的空框。
		a.applyRoundRegion()
		if a.panel != nil && a.panelUp.Load() {
			a.refreshBacking(true)
			a.panel.Update(func() {})
			a.refreshShadow()
		}
		// 3) 设置窗、文件互传窗。它们往往正是用户改完设置时盯着的那扇窗：只刷面板
		//    的话，屏幕上"什么都没变"，看着就是"点保存没反应"。
		if a.settings != nil && a.settings.glass != nil {
			a.settings.glass.refreshAppearance()
		}
		if a.share != nil && a.share.glass != nil {
			a.share.glass.refreshAppearance()
		}
	})
}

func (a *application) ready() {
	a.icons = map[string]*ui.Bitmap{}
	// 面板这块玻璃的共享状态。name 留空：日志保持原来的 "玻璃: 底图 …" 字样
	// （子窗那边是 "玻璃: 设置 底图 …"）。gate 是 nil —— 面板不靠 visible/hostShown
	// 门控投影，它的开关是 panelUp 和 hidePanel 里的显式调用。
	a.pane = paneState{ap: a, spec: a.panelShadowSpec}
	// 阴影层先建好（隐藏状态）。放在最前面：后面的窗口初始化里不会用到它，
	// 但唤出时一定要有，晚建的话第一帧就没影。
	a.pane.shadow = win.NewShadow()
	// 显示器缩放只能在 mygo 起来之后问（App 初始化前调 Screen.Displays 会 panic），
	// 所以放这里算一次缓存住，视图里不再现问。
	a.scale = screenScale()

	// 面板尺寸用配置里的值（默认 720×480）。它不再能靠拖边缘改 —— 见下面
	// Resizable 的说明；要改大小直接改配置。
	pw, ph := a.cfg.PanelSize()
	a.panel = mygo.NewWindow(mygo.WindowOptions{
		Title:  "Baozi",
		Panel:  true,
		Hidden: true,
		// 不用 Frameless：Windows 上无边框窗口既没有缩放边缘，也拿不到 DWM 的
		// 阴影和圆角。这里用"隐藏标题栏 + 隐藏窗口按钮"把阴影和圆角拿到手。
		// 保留四边（含顶边）的缩放边缘：这是"面板大小拖边改"的唯一入口。
		// 注意 internal/win 的窗口区域必须铺满**整个窗口**（含这圈边框带），
		// 否则右边和下边的缩放热区会被切掉，那里就拉不动大小了。
		TitleBarStyle:     mygo.TitleBarHidden,
		HideWindowButtons: true,
		Transparent:       true,
		// 这里**不设 Vibrancy**。Win10 上 mygo 的 vibrancy 本来就没有视觉作用（系统
		// backdrop 只有 Win11 22H2 起才有），但它会让 mygo 自己的 SetHasShadow 直接
		// return（那边写着 `if !w.captionless() || w.vibrancy != "" { return }`），
		// 连 DwmExtendFrameIntoClientArea 那一下都拿不到。面板的阴影现在是自绘的
		// （见 internal/win/shadow.go 与 glass.go 的 refreshShadow），所以这里既不需要
		// 它、也不该让它多走一条材质路径。
		CornerRadius: glassRadius,
		Width:        pw,
		Height:       ph,
		MinWidth:     config.MinPanelWidth,
		MinHeight:    config.MinPanelHeight,
		Content:      ui.View(a.view),
	})
	// 面板窗口交到玻璃状态手里：paneState 之后所有"客户区/窗口矩形"都从这里取。
	a.pane.win = a.panel
	// 用户拖着面板边缘改大小：停手 800ms 后把尺寸写进 config.json，
	// 下次启动按这个尺寸建窗。不用 mygo 的 StateKey——那个只在窗口真正关闭时落盘，
	// 而 Baozi 平时"关闭"只是隐藏（OnClose 里 PreventDefault），记不住。
	a.panel.OnResize(func() {
		t0 := time.Now()
		a.uiTick.Store(t0.UnixNano())
		a.dragUntil.Store(t0.Add(600 * time.Millisecond).UnixNano())
		// 先把这一帧的节奏记下来（见 resizeFrames 的注释）。
		a.panelMu.Lock()
		if a.resizeFrames == 0 {
			a.resizeStartedAt = t0
			a.resizeGapMax = 0
		}
		if !a.resizePrev.IsZero() {
			if gap := t0.Sub(a.resizePrev); gap > a.resizeGapMax {
				a.resizeGapMax = gap
			}
		}
		a.resizePrev = t0
		a.resizeFrames++
		a.panelMu.Unlock()
		// 缩放期间把阴影层收起来（只留面板自己 + 每帧重设的圆角区域）。
		//
		// 也试过"每帧重建"，两个问题：
		//   1) 每帧多一次位图重画 + UpdateLayeredWindow + SetWindowPos(TOPMOST)，
		//      缩放回调用实测从 5~6ms 涨到 8~10ms —— 用户对跟手最敏感，不值；
		//   2) 面板自己的图层在那一帧还没按新尺寸重排，四角补的桌面色/边框弧会
		//      错位露在玻璃外面，屏幕上就是"缩放时能看到四角画了道弧"。
		// 收起只需一次 ShowWindow，停手 160ms 由 finishResize 按最终尺寸重建。
		// （阴影"常驻"说的是空闲状态：唤出后一直有影，不是拖动的那 0.x 秒。）
		a.hideShadow()
		// 窗口区域**每帧都要跟着重设**：形状不跟着窗口走的话，拖动过程中四角就是直的
		// （而且旧区域还会把窗口新长出来的部分裁掉）。这一步只是一次
		// CreateRoundRectRgn + SetWindowRgn，实测不到 1ms。
		a.applyRoundRegion()
		// 四角月牙则**不能每帧重造**：每帧要合成四张图、上传四张 GPU 纹理。
		// 月牙的边长只由圆角半径决定、和面板尺寸无关，缩放期间沿用旧图完全正确
		// （只有桌面色可能略旧）。等停手 160ms 再重画一次。
		a.panelMu.Lock()
		if a.cornerTimer != nil {
			a.cornerTimer.Stop()
		}
		a.cornerTimer = time.AfterFunc(160*time.Millisecond, a.finishResize)
		a.panelMu.Unlock()
		if d := time.Since(t0); d > 6*time.Millisecond {
			log.Printf("缩放: 回调 %.2fms（超过 6ms 才记）", float64(d.Microseconds())/1000)
		}
		a.panelMu.Lock()
		if d := time.Since(t0); d > a.resizeMax {
			a.resizeMax = d
		}
		a.resizeSum += time.Since(t0)
		a.panelMu.Unlock()
		// 这里**不再调 panel.Update**：mygo 的 surface 收到 WM_SIZE 就自己
		// drawNow 了（surface.go："Draw at once, so the content follows the
		// window while it is resized"），玻璃的取景偏移也是在那一趟布局里按新
		// 客户区重算的。之前这里多调一次 Update，等于一帧画两遍 —— 一次布局加
		// 一次合成，缩放时两个都排在 UI 线程上，手就跟着卡。
		a.panelGeometryChanged()
	})
	// 拖面板本身（见 view 里给根容器挂的 DragWindow）同样停手 800ms 后记位置。
	a.panel.OnMove(func() {
		a.uiTick.Store(time.Now().UnixNano())
		a.dragUntil.Store(time.Now().Add(600 * time.Millisecond).UnixNano())
		// 自绘拖动（dragwin.go）自己会按目标位置烘图、挪窗口、提交一帧，位置和
		// 内容是同一帧交出去的 —— 这里不能再插一脚，否则一次移动画两遍。
		if a.selfDrag != nil && a.selfDrag.active {
			return
		}
		// 底图平移必须**当帧**做。以前这里只排一次 panel.Update，等那一帧布局跑完
		// 才在 view 里重算 —— 窗口已经被系统挪到新位置、图还是旧的，屏幕上就是
		// "拖动开始的一瞬间背景慢一拍"。增量平移只需要 1~2ms（整张重烘仍然受下面的
		// 16ms 节流和"一次只排一帧"约束），所以在 OnMove 里直接做。
		a.refreshBacking(false)
		// 光算完底图还不算"跟手"：拖动期间主线程在系统的模态拖动循环里，帧请求
		// 都在排队，屏幕上是上一帧。RedrawWindow(UPDATENOW) 返回前就会把 WM_PAINT
		// 派发掉，mygo 的 surface 收到就画一帧 —— 窗口挪到哪，玻璃同一刻跟到哪。
		// 节流 16ms：这一帧是整张内容的重画，实测都在 10ms 以内（超过会打日志）——
		// 跟手优先，宁可每帧多几毫秒，也别让玻璃落在窗口后头。
		if t := time.Now(); t.UnixNano() >= a.redrawNext.Load() {
			a.redrawNext.Store(t.Add(16 * time.Millisecond).UnixNano())
			if hwnd := a.panel.NativeHandle(); hwnd != 0 {
				t0 := time.Now()
				win.RedrawNow(hwnd)
				if d := time.Since(t0); d > 10*time.Millisecond {
					log.Printf("玻璃: 拖动重绘一帧 %.1fms", float64(d.Microseconds())/1000)
				}
			}
		}
		// 玻璃跟着面板走：整屏磨砂图不动，只改窗口在图上取景的偏移。
		//
		// 但**不能每帧都重建**：panel.Update 是"整棵内容树重新布局 + 重新栅格化
		// + 上传纹理"，1100×724 的面板一次就是几毫秒到十几毫秒，全砸在 UI 线程上；
		// 鼠标每秒能报几百次位移，那就等于把 UI 线程占满 —— 用户说的"快速拖动
		// 卡死"就出在这里。
		//
		// 磨砂底图现在**不再整张重烘**（见 backing.go 的增量平移），一次一两毫秒，
		// 所以节流从 50ms 收到 16ms：面板挪到哪，底图同一帧就跟到哪，不再"每 50ms
		// 跳一下"——那正是用户说的"模糊背景的移动有滞后"。
		now := time.Now()
		a.panelMu.Lock()
		// 正好在缩放（拖边）时，位置也在变：那一路的 mygo surface 收到 WM_SIZE
		// 就自己 drawNow 了，这里再排一次 Update 等于一帧画两遍 —— 之前"快速
		// 缩放卡死"就是这么来的。缩放的帧由 resize 回调管（见上面那段），
		// 这里只在**纯移动**时补帧。
		resizing := !a.resizePrev.IsZero() && now.Sub(a.resizePrev) < 200*time.Millisecond
		// 一次只排一个：拖面板走的是系统的模态拖动循环（mygo 的 DragWindow 就是
		// SendMessage(WM_NCLBUTTONDOWN, HTCAPTION)），那段循环里队列跑得比平时慢，
		// 每帧无条件 post 一个"整树重建"只会在队列里堆起来。排队了就不再排。
		queued := false
		if !resizing && !a.moveUpdateQueued && now.Sub(a.lastMoveAt) >= 16*time.Millisecond {
			a.lastMoveAt = now
			a.moveUpdateQueued = true
			queued = true
		}
		a.panelMu.Unlock()
		if queued {
			a.panel.Update(func() {
				a.panelMu.Lock()
				a.moveUpdateQueued = false
				a.panelMu.Unlock()
			})
		}
		a.panelMu.Lock()
		if a.moveTimer != nil {
			a.moveTimer.Stop()
		}
		a.moveTimer = time.AfterFunc(120*time.Millisecond, func() {
			if a.panel != nil && a.panel.IsVisible() {
				a.panel.Update(func() {})
			}
		})
		a.panelMu.Unlock()
		// 阴影层也要跟着挪，否则松手前它会停在原地。只挪位置、不重算位图
		// （尺寸没变），所以开销就是一次 UpdateLayeredWindow。
		a.followShadow()
		a.panelGeometryChanged()
	})
	// 阴影层的收起**不挂在 OnHide 上**：mygo 藏/显窗口用的是 DWMWA_CLOAK，Show 的
	// 过程里也会报一次 hide，挂上去就成了"刚显示又被收掉"（实测：面板在，图层
	// IsWindowVisible=False）。所有收面板的地方都走 a.hidePanel()。
	a.panel.OnClose(func(e *mygo.CloseEvent) {
		// 真正退出时不能拦：mygo 的 Quit 是一个个关窗口，
		// 只要有窗口在 OnClose 里 PreventDefault，整个 Quit 就被取消，
		// 表现就是"点了退出，进程还在"。
		if a.quitting.Load() {
			return
		}
		e.PreventDefault()
		a.hidePanel()
	})
	a.panel.OnBlur(a.hidePanel)

	// 1) 索引：先用上次快照秒开，再按配置决定是否重扫
	a.bootIndex()

	// 2) 应用列表：读开始菜单，几十毫秒。拼音串在这里一次算好。
	a.startWatchdog()
	a.appsReloading.Store(true)
	go a.loadApps(true)

	// 3) 钩子：双击 Ctrl 走低级键盘钩子，组合键走 RegisterHotKey 兜底
	a.startHook()
	if a.cfg.Hotkey.Combo != "" {
		if err := mygo.GlobalShortcut.Register(a.cfg.Hotkey.Combo, a.toggle); err != nil {
			log.Printf("注册组合键 %s 失败: %v", a.cfg.Hotkey.Combo, err)
		}
	}

	// 开机自启的开关状态问系统一次（后面托盘/设置都读缓存，不再走主循环）。
	a.openAtLogin.Store(mygo.App.OpenAtLogin())
	log.Printf("开机自启: %v", a.openAtLogin.Load())

	a.setUpTray()
	a.settings = newSettingsWindow(a)
	a.share = newShareWindow(a)

	// 更新检查：起一个 goroutine 就返回。它自己先睡 3 秒再发请求，
	// 所以"启动"这条路上没有任何一行代码在等网络（见 updatecheck.go）。
	// 代理开关先按配置落定，否则第一次检查会用缺省（开），与用户的选择不一致。
	update.SetUseProxy(a.cfg.UpdatesUseProxy())
	a.startUpdateCheck()

	// 文件对话框跟随：记住文件管理器最近待过的目录，那个「打开 / 另存为」框再回到前台时
	// 自动把路径送过去（Lertaro 的 IActivePathCollector + FileDialogNavigationTracker）。
	// 目录来源七种（资源管理器 / TC / Opus / XYplorer / xplorer2 / tessoa / Arke），设置里可逐个关掉；
	// 关掉总开关（config.dialogFollow = false）就完全不碰别人的窗口。
	a.syncFollow()

	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		a.shutdown()
	})
	// 开机自启拉起时不弹面板，直接待在托盘（静默启动）。
	// 设置里的"启动时自动隐藏"让手动启动也走这条路（只留托盘 + 双击 Ctrl）。
	// BAOZI_HEADLESS=1 给冒烟测试留下同一条路径，不用真去改注册表。
	autoLogin := mygo.App.WasOpenedAtLogin()
	if !autoLogin && !a.cfg.HideOnStart() && os.Getenv("BAOZI_HEADLESS") == "" {
		a.show()
	} else if autoLogin {
		log.Printf("启动：来自开机自启，面板保持隐藏（托盘可用，双击 Ctrl 随时唤出）")
	} else {
		log.Printf("启动：按设置保持隐藏（托盘可用，双击 Ctrl 随时唤出）")
	}
	// 命令行直达某个窗口：BAOZI_OPEN=settings / share。
	// 必须延迟到事件循环里再 Show：窗口刚创建完时 Show 会被忽略（表现为窗口存在但一直隐藏）。
	if want := strings.ToLower(os.Getenv("BAOZI_OPEN")); want != "" {
		time.AfterFunc(700*time.Millisecond, func() {
			a.panel.Update(func() {
				switch want {
				case "settings":
					a.openSettings()
				case "share":
					a.openShare()
				}
			})
		})
	}

	// BAOZI_MEMSTAT=10s：周期性打内存数字，调内存时用（默认不开）。
	if d := os.Getenv("BAOZI_MEMSTAT"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil && dur > 0 {
			go func() {
				for range time.Tick(dur) {
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					log.Printf("内存: heap=%.0fMB inuse=%.0fMB sys=%.0fMB objs=%d gc=%d",
						float64(m.HeapAlloc)/(1<<20), float64(m.HeapInuse)/(1<<20),
						float64(m.HeapSys)/(1<<20), m.HeapObjects, m.NumGC)
				}
			}()
		}
	}

	// BAOZI_PPROF=1：开一个 127.0.0.1:6060 的 pprof 端点，查内存/CPU 真实去向。
	if os.Getenv("BAOZI_PPROF") != "" {
		go func() {
			log.Printf("pprof: http://127.0.0.1:6060/debug/pprof/")
			if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
				log.Printf("pprof: %v", err)
			}
		}()
	}

	// BAOZI_UNINSTALL=1：无人值守卸载（摘自启 + 删数据目录 + 退出），跳过确认框。
	// 给冒烟和"托盘点不出来时"留一条命令行退路；正常用户走托盘那一项。
	if os.Getenv("BAOZI_UNINSTALL") != "" {
		time.AfterFunc(time.Second, a.doUninstall)
	}

	// BAOZI_AUTOQUIT=2s：冒烟用，到点走一遍真实退出流程（和托盘「退出」同一条路）。
	if d := os.Getenv("BAOZI_AUTOQUIT"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			time.AfterFunc(dur, func() {
				a.panel.Update(func() { a.quit() })
			})
		}
	}
}

// indexFile 返回当前这一代索引快照的路径。
// 从 v4 起服务端每重建一次就可能写到新文件（index-<时间戳>.bin）：
// 主进程用 mmap 长期映射着上一代，Windows 不允许替换被映射的文件，
// 所以"哪一份是最新的"要靠目录里按 mtime 挑，而不是写死 index.bin。
func (a *application) indexFile() string {
	if a.idxPath != "" {
		return a.idxPath
	}
	if p, _, err := index.Latest(a.dataDir); err == nil {
		return p
	}
	return filepath.Join(a.dataDir, "index.bin")
}
func (a *application) historyFile() string { return filepath.Join(a.dataDir, "history.json") }

func (a *application) bootIndex() {
	alive := a.serviceAlive()
	path := a.indexFile()
	tab, roots, err := index.Load(path)
	n := 0
	if tab != nil {
		n = tab.Len()
	}
	if err == nil && n > 0 {
		a.idxReplace(tab)
		a.idxPath = path
		// 记下这份快照的"代次"，好让增量知道能不能往上叠
		if info, e := os.Stat(path); e == nil {
			a.idxMtime = info.ModTime()
		}
		// 顺手收掉更老的代次：映射正被自己占用的删不掉，那没关系，
		// 下次启动时（还没映射）就能删干净
		index.Prune(a.dataDir, path)
		a.idxBase = readStateBase(a.dataDir)
		a.idxSeq = 0
		a.idxOps = 0
		if alive {
			a.setStatus(fmt.Sprintf("%d 项 · USN 索引", n))
			log.Printf("索引: 服务模式，%d 项（来自 %s）", n, a.indexFile())
		} else {
			a.setStatus(fmt.Sprintf("%d 项 · 索引已就绪", n))
			log.Printf("索引: 快照 %d 项（目录扫描模式）", n)
		}
	} else {
		if err != nil && !os.IsNotExist(err) {
			log.Printf("读取索引: %v", err)
		}
		if alive {
			a.setStatus("索引服务正在建索引…")
		} else {
			a.setStatus("正在建立索引…")
		}
	}
	needScan := n == 0 || rootsChanged(roots, a.cfg.Index.Roots)
	// BAOZI_REINDEX=1 强制重建一次（改了索引规则/排除了新目录后用，不必删索引文件）。
	if os.Getenv("BAOZI_REINDEX") != "" {
		needScan = true
	}
	// 服务在跑的时候索引归它维护：主进程既不自己扫，也不去覆盖它刚写出来的快照。
	if alive {
		needScan = false
	}
	if !needScan && !alive && a.cfg.Index.RescanMinutes > 0 {
		if info, err := os.Stat(a.indexFile()); err == nil {
			needScan = time.Since(info.ModTime()) > time.Duration(a.cfg.Index.RescanMinutes)*time.Minute
		}
	}
	if needScan {
		go a.rescan()
	} else if n > 0 {
		// 命中上次快照：解压时的临时缓冲已经用完，把空闲页还回去，
		// 否则任务管理器会长期显示那个加载峰值。
		releaseMemory()
	}
	// 无论服务当前在不在跑都挂上监听：用户随时可能在设置里把它打开，
	// 打开后心跳一出现，这里就自动接管（自己扫的时候它会按心跳门控跳过）。
	a.watchIndexFile()
	// 启动时立刻追一次增量：index.bin 可能是几小时前写的，
	// 服务攒下来的新条目都在 index.delta 里（这一步只叠指令，不重新读盘）。
	if alive {
		go a.refreshIndex(false)
	}
}

// rootsChanged 判断索引快照覆盖的根目录是否与当前配置一致。
func rootsChanged(old, want []string) bool {
	if len(old) != len(want) {
		return true
	}
	for i := range old {
		if !strings.EqualFold(old[i], want[i]) {
			return true
		}
	}
	return false
}

// excludesChanged 判断排除规则有没有实质变化。和 rootsChanged 一样按集合比：
// 用户多敲一个空行、或者把两条换个顺序，都不该触发一次分钟级的全量重建。
// 大小写也无关 —— 规则匹配本来就是大小写不敏感的。
func excludesChanged(old, want []string) bool {
	old = nonEmptyTrimmed(old)
	want = nonEmptyTrimmed(want)
	if len(old) != len(want) {
		return true
	}
	oldSet := make(map[string]int, len(old))
	for _, s := range old {
		oldSet[s]++
	}
	for _, s := range want {
		if oldSet[s] == 0 {
			return true
		}
		oldSet[s]--
	}
	return false
}

// nonEmptyTrimmed 去掉空白项并统一小写，供集合比较用。
func nonEmptyTrimmed(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// idxReplace 换索引快照。名字 arena 和 nodes 一起换，旧快照被替换后依然完整。
func (a *application) idxReplace(t *index.Table) {
	a.cat.Store(t)
}

// rescan 后台全量扫描；结束落盘，供下次秒开。
func (a *application) rescan() {
	// 索引服务在跑时索引归它管：这里硬扫一遍既慢（WalkDir 分钟级），
	// 写回去的快照还会被服务的下一轮覆盖。改成给它放一个"现在就重建"的信标。
	if a.serviceAlive() {
		if err := os.WriteFile(filepath.Join(a.dataDir, "index.rebuild"), []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
			log.Printf("请求服务重建: %v", err)
		}
		a.setStatus("已请求索引服务重建")
		return
	}

	a.scanWG.Add(1)
	defer a.scanWG.Done()

	a.scanMu.Lock()
	if a.scanOff != nil {
		a.scanOff()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.scanOff = cancel
	a.scanMu.Unlock()
	defer cancel()

	start := time.Now()
	progress := func(n int) {
		a.panel.Update(func() { a.status = fmt.Sprintf("正在建立索引… %d 项", n) })
	}
	tab, err := index.Build(ctx, a.cfg.Index, progress)
	if err != nil && ctx.Err() == nil {
		log.Printf("建立索引: %v", err)
		a.setStatus("索引失败，见日志")
		return
	}
	// 被取消（退出、卸载、或又点了一次重建）时不能落盘：
	// 半份索引一旦写进索引文件，下次启动会当完整索引加载，搜出来就缺东西。
	// 卸载时这条尤其致命——RemoveAll 删完目录，这里又把索引文件写回来。
	if ctx.Err() != nil {
		return
	}
	if tab.Len() == 0 {
		return
	}
	a.idxReplace(tab)
	if path, err := index.Save(a.indexFile(), tab, a.cfg.Index.Roots); err != nil {
		log.Printf("保存索引: %v", err)
	} else {
		a.idxPath = path
	}
	a.setStatus(fmt.Sprintf("%d 项 · 扫描 %s", tab.Len(), time.Since(start).Round(time.Second)))
	releaseMemory()
}

// releaseMemory 把加载/扫描期的临时峰值还给系统。
// Go 拿到 OS 的内存不会主动退：179 万条的 gob 解码会堆出几百 MB 垃圾，
// 不在关键节点踩一脚 FreeOSMemory，任务管理器就一直显示那个峰值。
// 延迟 2s 再跑，别让 GC 的 STW 卡住首帧。
func releaseMemory() {
	time.AfterFunc(2*time.Second, func() {
		debug.FreeOSMemory()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Printf("内存: 已归还空闲页，heap=%.0fMB sys=%.0fMB",
			float64(m.HeapAlloc)/(1<<20), float64(m.HeapSys)/(1<<20))
	})
}

func (a *application) setStatus(s string) {
	if a.panel == nil {
		a.status = s
		return
	}
	a.panel.Update(func() { a.status = s })
}

// startHook 拉起 baozi-hook.exe，读它的 stdout。
func (a *application) startHook() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("定位自身: %v", err)
		return
	}
	hookPath := filepath.Join(filepath.Dir(exe), "baozi-hook.exe")
	if _, err := os.Stat(hookPath); err != nil {
		a.setStatus("提示：未找到 baozi-hook.exe，双击 Ctrl 不可用")
		return
	}
	cmd := exec.Command(hookPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("钩子 stdin: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("钩子 stdout: %v", err)
		return
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("启动钩子: %v", err)
		a.setStatus("钩子启动失败，双击 Ctrl 不可用")
		return
	}
	a.hook, a.hookIn = cmd, stdin

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			switch strings.TrimSpace(scanner.Text()) {
			case "toggle":
				a.toggle()
			case "ready":
				a.setStatus("双击 Ctrl 已就绪")
			case "error":
				a.setStatus("钩子初始化失败")
			}
		}
	}()
}

// syncFollow 按当前配置启停跟随器，并刷新"哪些来源开着"的快照。
// lastFollowDir 返回跟随器记住的文件管理器目录，供命令里的 {dir} 用。
// 跟随没开（或还没读到目录）时是空串——占位符就替换成空，不留一个假的路径。
func (a *application) lastFollowDir() string {
	if a.follow == nil {
		return ""
	}
	return a.follow.LastDir()
}

// 设置页保存后调它：总开关一改就真的停/起，来源勾选改了下次轮询立刻生效。
func (a *application) syncFollow() {
	a.refreshPathSources()

	if !a.cfg.DialogFollowOn() {
		if a.followCancel != nil {
			a.followCancel()
			a.followCancel = nil
			a.follow = nil
			log.Print("对话框跟随: 已停止")
		}
		return
	}
	if a.follow != nil {
		return // 已经在跑，来源开关通过 sources 快照热更新
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.followCancel = cancel
	a.follow = win.NewDialogFollow(func(ev win.FollowEvent) {
		log.Printf("对话框跟随: %#x -> %s", ev.HWND, ev.Dir)
	}, log.Printf, a.pathSourceOn)
	a.follow.Start(ctx)
	log.Print("对话框跟随: 已启动（来源：" + a.enabledSourceSummary() + "）")
}

// refreshPathSources 把当前配置里的来源开关拍成一份快照给跟随器读。
func (a *application) refreshPathSources() {
	m := make(map[string]bool, len(win.SourceNames()))
	for _, kind := range win.SourceNames() {
		m[kind] = a.cfg.PathSourceEnabled(kind)
	}
	a.sources.Store(&m)
}

// pathSourceOn 是给跟随器的查询接口：这个来源现在开不开。
func (a *application) pathSourceOn(kind string) bool {
	m := a.sources.Load()
	if m == nil {
		return true
	}
	return (*m)[kind]
}

// enabledSourceSummary 只用于日志：把开着的来源列出来，关掉的标出来。
func (a *application) enabledSourceSummary() string {
	parts := make([]string, 0, len(win.SourceNames()))
	for _, kind := range win.SourceNames() {
		if a.pathSourceOn(kind) {
			parts = append(parts, win.SourceLabel(kind))
		}
	}
	if len(parts) == 0 {
		return "无（只剩跟随框架，不会读任何窗口）"
	}
	return strings.Join(parts, " / ")
}

func (a *application) shutdown() {
	a.quitOnce.Do(func() {
		log.Printf("收尾: 停掉对话框跟随")
		if a.followCancel != nil {
			a.followCancel()
		}
		log.Printf("收尾: 取消索引扫描")
		if a.scanOff != nil {
			a.scanOff()
		}
		log.Printf("收尾: 通知钩子退出")
		if a.hookIn != nil {
			_ = a.hookIn.Close() // 钩子读到 EOF 自己退出
		}
		if a.hook != nil && a.hook.Process != nil {
			done := make(chan struct{})
			go func() { _ = a.hook.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = a.hook.Process.Kill()
			}
		}
		log.Printf("收尾: 历史落盘")
		// 卸载时跳过：数据目录马上就要删掉，落盘只会把 history.json 又写回来。
		if a.hist != nil && !a.uninstalling.Load() {
			if err := a.hist.Flush(); err != nil {
				log.Printf("保存历史: %v", err)
			}
		}
		log.Printf("收尾: 完成")
	})
}

func (a *application) setUpTray() {
	tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayIcon(), ToolTip: "Baozi"})
	if err != nil {
		log.Print("托盘不可用: ", err)
		return
	}
	a.tray = tray
	menu := mygo.NewMenu([]*mygo.MenuItem{
		{Label: "显示 Baozi", Accelerator: a.cfg.Hotkey.Combo, Click: func(*mygo.MenuItem, *mygo.Window) { a.show() }},
		{Label: "文件互传…", Click: func(*mygo.MenuItem, *mygo.Window) { a.openShare() }},
		{Label: "设置…", Click: func(*mygo.MenuItem, *mygo.Window) { a.openSettings() }},
		mygo.Separator(),
		{Label: "重新建立索引", Click: func(*mygo.MenuItem, *mygo.Window) { go a.rescan() }},
		{Label: "打开配置文件", Click: func(*mygo.MenuItem, *mygo.Window) {
			if err := mygo.Shell.OpenPath(a.cfgPath); err != nil {
				log.Printf("打开配置: %v", err)
			}
		}},
		mygo.Separator(),
		{Label: "开机启动", Type: mygo.MenuItemCheckbox, ID: "login", Checked: a.openAtLogin.Load(),
			Click: func(it *mygo.MenuItem, _ *mygo.Window) {
				if err := mygo.App.SetOpenAtLogin(it.Checked); err != nil {
					log.Printf("开机自启: %v", err)
					// 失败就把勾选拨回去：mygo 已经把状态翻过了，不纠正会骗人。
					it.SetChecked(!it.Checked)
					return
				}
				a.openAtLogin.Store(it.Checked)
				log.Printf("开机自启: %v", it.Checked)
				// 设置窗口开着的话，勾选状态要跟着变。
				if a.settings != nil {
					a.settings.load()
					a.settings.win.Update(func() {})
				}
			}},
		mygo.Separator(),
		// 卸载：摘自启 + 删数据目录，程序文件留着。会先弹系统确认框。
		// 用 go 起 goroutine：确认框是模态的，别把主线程的消息循环占了。
		{Label: "卸载：清除自启与数据…", Click: func(*mygo.MenuItem, *mygo.Window) { go a.uninstall() }},
		mygo.Separator(),
		// 不用 RoleQuit：它的行为绑在菜单的角色机制上，出问题时既看不到日志也不好接管。
		// 这里显式收口到 a.quit()，退出前后的动作都自己掌控。
		{Label: "退出 Baozi", Click: func(*mygo.MenuItem, *mygo.Window) { a.quit() }},
	})
	a.tray.SetMenu(menu)
	a.menu = menu
	// 托盘图标要在 native 调用还活着的时候删掉，所以放在 OnWillQuit（不是 OnQuit）。
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		a.shutdown()
		if a.tray != nil {
			a.tray.Destroy()
		}
	})
}

// quit 是唯一的退出入口：托盘「退出」走这里，BAOZI_AUTOQUIT 也走这里。
// 先把 quitting 置真（让窗口的 OnClose 放行），再做收尾，最后让 mygo 关窗口停循环。
func (a *application) quit() {
	log.Printf("退出：开始收尾")
	a.quitting.Store(true)
	a.shutdown()
	log.Printf("退出：收尾完成，请求关闭窗口")
	mygo.App.Quit()
}

func main() {
	// `baozi.exe --update-now`：不建窗口，直接查 + 装 + 退出（脚本会覆盖并重启）。
	// 放在单实例锁之前 —— 它本来就是个一次性的短命进程，不该被已经在跑的那份挡住。
	if handleUpdateFlag(os.Args[1:]) {
		return
	}
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	// 常驻后台的小工具，堆不该按 GOGC=100 涨到活数据的两倍。
	// 索引是只读的（整表替换），存活量基本恒定，压低 GOGC 只是用一点 CPU 换几百 MB。
	debug.SetGCPercent(40)
	app := &application{icons: map[string]*ui.Bitmap{}, dragRetryLeft: 8}
	setupLog()

	cfg, cfgPath, err := config.Load()
	if err != nil {
		log.Fatalf("读取配置: %v", err)
	}
	dir, err := config.Dir()
	if err != nil {
		log.Fatalf("数据目录: %v", err)
	}
	app.cfg, app.cfgPath, app.dataDir = cfg, cfgPath, dir
	// 内置脚本（关机/睡眠）在这里落盘，并把配置里指向旧外部路径的命令改成
	// builtin:shutdown：脚本随程序走，不再依赖用户自己目录里的那份拷贝。
	if ensureBuiltinCommands(app.cfg) {
		if err := config.Save(app.cfg, app.cfgPath); err != nil {
			log.Printf("保存配置（内置命令迁移）: %v", err)
		}
	}
	// 外观必须在建面板窗口之前落地：圆角半径会作为 WindowOptions.CornerRadius
	// 传给 mygo（窗口级参数，只在创建时读一次），晚一步就得改完重启才生效。
	applyAppearance(cfg)

	hist, err := history.Open(app.historyFile(), cfg.History.MaxItems, 400*time.Millisecond)
	if err != nil {
		log.Fatalf("读取历史: %v", err)
	}
	app.hist = hist

	mygo.App.SetName("Baozi")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.OnSecondInstance(func([]string, string) { app.show() })
	mygo.App.WhenReady(app.ready)

	log.Printf("配置: %s", cfgPath)
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// dualWriter 把日志同时送到 stderr 和日志文件。
// 打包成 windowsgui 后进程没有控制台，stderr 写不进去也不能让写文件跟着失败。
type dualWriter struct{ f *os.File }

func (d dualWriter) Write(p []byte) (int, error) {
	if d.f != nil {
		_, _ = d.f.Write(p)
	}
	_, _ = os.Stderr.Write(p)
	return len(p), nil
}

// logFile 是当前日志文件句柄。卸载时要先关掉它：Windows 不删正在写入的文件，
// 留着句柄 RemoveAll 数据目录会失败（或删一半）。
var logFile *os.File

// setupLog 给 GUI 版本留一个能查的日志：%LOCALAPPDATA%\Baozi\baozi.log。
// 超过 2MB 直接清空重来（不做轮转，这个文件只用来排障）。
func setupLog() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "baozi.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 2<<20 {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	logFile = f
	log.SetOutput(dualWriter{f: f})
}

// closeLog 放开日志句柄。之后的日志只走 stderr（GUI 子系统下等于丢弃，此时进程也快退了）。
func closeLog() {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	log.SetOutput(os.Stderr)
}
