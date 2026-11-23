// Package config 读取用户配置。目标是"像 Listary 一样简单"：
// 只有一个 config.json，所有项都有合理默认值，缺项不用管。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是全部配置。字段与 JSON 一一对应，缺省项使用 Default() 的值。
type Config struct {
	Hotkey   Hotkey    `json:"hotkey"`
	Search   Search    `json:"search"`
	Index    Index     `json:"index"`
	History  History   `json:"history"`
	Share    Share     `json:"share"`
	Panel    Panel     `json:"panel"`
	Commands []Command `json:"commands"`
	// DialogFollow 打开"文件对话框跟着文件管理器走"：另存为框开着时去资源管理器换目录，
	// 再切回那个框，路径自动变成新目录（Lertaro / Listary 的行为）。
	// 用指针是为了区分"没写"（默认开）和"写了 false"（明确关掉，Baozi 就不再读别人的窗口）。
	DialogFollow *bool `json:"dialogFollow,omitempty"`
	// PathSources 指定从哪些文件管理器取当前目录。每项缺省为开：
	// 老配置里没有这一块，用户已有的行为不变。
	PathSources *PathSources `json:"pathSources,omitempty"`
	// Appearance 是面板外观（圆角 / 玻璃不透明度 / 投影）。三项都能单独关掉、
	// 单独调程度；缺省值就是"现在的样子"，老配置读进来什么都不变。
	Appearance Appearance `json:"appearance"`
	// Startup 是启动时的行为（手动启动后要不要直接把面板弹出来）。
	Startup Startup `json:"startup"`
	// Updates 管"关于"页里的自动检查更新（缺省开）。
	Updates Updates `json:"updates"`
}

// Appearance 是面板外观：圆角、玻璃不透明度、投影强度。
// 每一项都是"开关 + 程度"：关掉就是不要这项效果，程度决定做到多少。
type Appearance struct {
	// Radius 是圆角半径（DIP，0~40，缺省 24）。关掉 = 直角。
	Radius Toggle `json:"radius"`
	// Opacity 是玻璃的**透明度**（百分比，缺省 59）：越大越透 —— 桌面越清楚，
	// 磨砂的模糊也跟着变轻；0 = 完全不透明（看不见桌面，只有磨砂白）。
	// 即使拉到 100 也留着一层白打底（见 main.go 的 applyAppearance），
	// 否则面板糊成"直接看桌面"，字压不住、面板也没了边界。
	Opacity Toggle `json:"opacity"`
	// Shadow 是面板外那圈投影的强度（百分比，缺省 57）：0 等于没有投影。
	Shadow Toggle `json:"shadow"`
}

// Toggle 是"开关 + 程度"这一对值。
//
// 两个字段都必须是指针：零值结构要能表示"用户没配过"。否则 0 到底是
// "用户要直角"还是"没配过"就分不清了 —— 前者该生效，后者该用默认值。
type Toggle struct {
	Enabled *bool `json:"enabled,omitempty"`
	Value   *int  `json:"value,omitempty"`
}

// On 报这一项是否启用；没配过按 def 算。
func (t Toggle) On(def bool) bool {
	if t.Enabled == nil {
		return def
	}
	return *t.Enabled
}

// Int 报程度值；没配过按 def 算，并夹进 lo..hi（挡掉手改配置写进去的离谱值）。
func (t Toggle) Int(def, lo, hi int) int {
	v := def
	if t.Value != nil {
		v = *t.Value
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

const (
	// DefaultPanelRadius 是缺省的圆角半径（DIP）。24 这条弧在 125% 缩放下
	// 正好 30 物理像素，是反复对过 iced 效果之后定下来的值。
	DefaultPanelRadius = 24
	// MaxPanelRadius 是半径上限：再大面板就成胶囊了。
	MaxPanelRadius = 40
	// DefaultGlassTransparency 是缺省的玻璃透明度（百分比）：压白 alpha ≈ 158、
	// 模糊半径 5 —— 就是之前那套"牛奶白磨砂"的观感。
	DefaultGlassTransparency = 59
	// DefaultShadowPct 是缺省的投影强度（百分比），对应黑度 ≈ 0.34。
	DefaultShadowPct = 57
	// DefaultDoubleCtrlIntervalMs 是两次 Ctrl 按下的缺省最大间隔（毫秒）。
	// 200ms 比 400ms 更接近"顺手连按两下"的节奏：400ms 太宽，Ctrl+C 之后
	// 隔了小半秒再按 Ctrl 会被误判成唤出。
	DefaultDoubleCtrlIntervalMs = 200
)

// PanelRadius 返回生效的圆角半径（DIP）；关掉返回 0，也就是直角。
func (c *Config) PanelRadius() int {
	if !c.Appearance.Radius.On(true) {
		return 0
	}
	return c.Appearance.Radius.Int(DefaultPanelRadius, 0, MaxPanelRadius)
}

// GlassTransparencyPct 返回生效的玻璃透明度（百分比 0~100）。
// 0 = 完全不透明；100 = 最透（实现里还有下限保护，不会真成全透明）。
// 把这一项的开关关掉，等于"不要玻璃看起来能看穿" —— 按 0（完全不透明）算。
func (c *Config) GlassTransparencyPct() int {
	if !c.Appearance.Opacity.On(true) {
		return 0
	}
	return c.Appearance.Opacity.Int(DefaultGlassTransparency, 0, 100)
}

// ShadowPct 返回生效的投影强度（百分比 0~100）；0 表示不要投影。
func (c *Config) ShadowPct() int {
	if !c.Appearance.Shadow.On(true) {
		return 0
	}
	return c.Appearance.Shadow.Int(DefaultShadowPct, 0, 100)
}

// Startup 是启动时的行为。
type Startup struct {
	// HideOnStart 为真时，启动后不弹面板，只在托盘里等着（双击 Ctrl 随时唤出）。
	// 开机自启拉起时本来就静默；这一项管的是"手动双击图标启动"的情况。
	HideOnStart *bool `json:"hideOnStart,omitempty"`
}

// HideOnStart 报启动时要不要保持隐藏。
func (c *Config) HideOnStart() bool {
	return c.Startup.HideOnStart != nil && *c.Startup.HideOnStart
}

// Updates 是"关于/更新"这块的偏好。真正的检查结果不落盘（每次启动都重新查），
// 这里只存"要不要自动查"。
type Updates struct {
	// AutoCheck 为真时，每次启动在后台查一次 GitHub Releases（缺省开）。
	// 检查永远是后台 goroutine + 超时，任何情况下都不许挡启动。
	AutoCheck *bool `json:"autoCheck,omitempty"`
	// UseProxy 为真时，更新检查/下载走代理（缺省开）。地址不落盘：
	// 每次打开设置都从环境变量/系统代理现读，用户换了端口不用改配置。
	UseProxy *bool `json:"useProxy,omitempty"`
}

// UpdateAutoCheck 报启动后要不要自动查新版本；没配过就是查。
func (c *Config) UpdateAutoCheck() bool {
	if c.Updates.AutoCheck == nil {
		return true
	}
	return *c.Updates.AutoCheck
}

// UpdatesUseProxy 报更新要不要走代理；没配过就是走（有就比没有强）。
func (c *Config) UpdatesUseProxy() bool {
	if c.Updates.UseProxy == nil {
		return true
	}
	return *c.Updates.UseProxy
}

// PathSources 是"文件对话框跟随"的目录来源开关，和 internal/win 的 Source* 常量一一对应。
// 用指针是为了区分"没写"（默认开）和"写了 false"（明确关掉这个来源）。
type PathSources struct {
	Explorer *bool `json:"explorer,omitempty"`
	TotalCmd *bool `json:"totalcmd,omitempty"`
	Opus     *bool `json:"opus,omitempty"`
	Xyplorer *bool `json:"xyplorer,omitempty"`
	Xplorer2 *bool `json:"xplorer2,omitempty"`
	// Tessoa 走它自己的 `--print-cwd`（自绘窗口读不到地址栏），详见 internal/win/tessoa.go。
	Tessoa *bool `json:"tessoa,omitempty"`
	// Arke 走它的 `--api -` state 查询（双栏窗口里按活动窗格给目录），详见 internal/win/arke.go。
	Arke *bool `json:"arke,omitempty"`
}

// DialogFollowOn 表示"文件对话框跟随"是否开启：配置里没写就是开（和产品默认一致）。
func (c *Config) DialogFollowOn() bool {
	if c.DialogFollow == nil {
		return true
	}
	return *c.DialogFollow
}

// PathSourceEnabled 报某个目录来源是否启用；没写就是启用。
// kind 取值与 internal/win 的 SourceExplorer / SourceTotalCmd / SourceOpus /
// SourceXyplorer / SourceXplorer2 一致。
func (c *Config) PathSourceEnabled(kind string) bool {
	p := c.PathSources
	if p == nil {
		return true
	}
	var v *bool
	switch kind {
	case "explorer":
		v = p.Explorer
	case "totalcmd":
		v = p.TotalCmd
	case "opus":
		v = p.Opus
	case "xyplorer":
		v = p.Xyplorer
	case "xplorer2":
		v = p.Xplorer2
	case "tessoa":
		v = p.Tessoa
	case "arke":
		v = p.Arke
	default:
		return true
	}
	return v == nil || *v
}

// Share 控制局域网互传。
type Share struct {
	// Bind 指定绑哪块网卡（填它的 IPv4 地址）。留空 = 所有网卡（默认）。
	// 机器上有 VMware / MuMu / Hyper-V 这类虚拟网卡时，自动挑选可能挑到它们，
	// 那种情况下在这里固定成实际联网的那块。
	Bind string `json:"bind"`
	// TTLMinutes 是分享链接的有效期（分钟），默认 60。到点自动失效——
	// 分享是一次性的临时通道，不做"只要不停止就一直挂着"。
	TTLMinutes int `json:"ttlMinutes"`
	// Signal 是跨网互传的信令服务地址，形如 https://baozi-signal.xxx.workers.dev。
	// 留空 = 没配跨网，界面上只给局域网那一半。
	//
	// 它只在建连时用一次：两端交换各自的 SDP，之后文件走 WebRTC 直连，不经过它。
	Signal string `json:"signal"`
}

// Hotkey 控制唤出方式。
type Hotkey struct {
	// DoubleCtrl 双击 Ctrl 唤出（Lertaro 的默认交互）。
	DoubleCtrl bool `json:"doubleCtrl"`
	// DoubleCtrlIntervalMs 两次 Ctrl 按下的最大间隔。
	DoubleCtrlIntervalMs int `json:"doubleCtrlIntervalMs"`
	// Combo 备用组合键，走 RegisterHotKey；留空表示不注册。
	Combo string `json:"combo"`
	// DisableInFullscreen 全屏应用（游戏/播放器）前台时不响应。
	DisableInFullscreen bool `json:"disableInFullscreen"`
}

// Search 控制结果排序。权重顺序即用户要的优先级：频次 > 类型 > 准确性。
type Search struct {
	Weights    Weights  `json:"weights"`
	TypeOrder  []string `json:"typeOrder"`
	MaxResults int      `json:"maxResults"`
	// Thumbnails 控制图片/视频在结果里显示内容缩略图还是类型线框图标。
	// 指针是为了区分"没配置过"（nil，按开处理）和"用户明确关掉"。
	Thumbnails *bool `json:"thumbnails,omitempty"`
	// IconScale 是图标大小的倍数：1 = 100%，也就是系统缩放下的默认样子
	// （100% 屏上是 22、125% 屏上是 27.5）。0（含没配置）按 100% 算。
	// 设置页给 50/75/100/125/150/175/200% 几个台阶，也能用滑杆调像素（夹在 0.5~3）。
	IconScale float64 `json:"iconScale,omitempty"`
	// IconSize 是旧字段（像素），只用于读老配置：升级时按"像素 ÷ 当时的默认大小"
	// 折算成倍数，老用户看到的图标大小不会跳一下。新配置写的是 IconScale。
	IconSize int `json:"iconSize,omitempty"`
}

// IconScale 的上下限：再小认不出是什么图标，再大名字就没地方放了。
const (
	MinIconScale = 0.5
	MaxIconScale = 3.0
)

// ClampIconScale 把倍数收进合法区间，挡掉手输的 0、负数、离谱大的值。
func ClampIconScale(k float64) float64 {
	if k < MinIconScale {
		return MinIconScale
	}
	if k > MaxIconScale {
		return MaxIconScale
	}
	return k
}

// BaseIconSize 是 100% 缩放下的默认图标边长。这个基数决定"默认看起来多大"，
// 缩放比例只负责把高 DPI 屏上的视觉尺寸找回来。
const BaseIconSize = 22

// IconSizeAt 返回在给定系统缩放下的图标边长。
//
// 三档取值，优先级从高到低：IconScale（倍数，新配置）→ IconSize（像素，老配置）
// → 什么都没有就按 100% 跟随系统缩放。
func (c *Config) IconSizeAt(scale float32) float32 {
	if scale <= 0 {
		scale = 1
	}
	if k := c.Search.IconScale; k > 0 {
		return BaseIconSize * scale * float32(ClampIconScale(k))
	}
	// 老配置里存的是像素：直接用它，别因为升级让老用户的图标大小变了一遍。
	if n := c.Search.IconSize; n > 0 {
		return float32(n)
	}
	return BaseIconSize * scale
}

// ThumbnailsOn 报告是否显示缩略图：没配置过默认开。
func (c *Config) ThumbnailsOn() bool {
	return c.Search.Thumbnails == nil || *c.Search.Thumbnails
}

// Panel 控制快速面板的尺寸。"快速面板"是一块固定大小、居中弹出的玻璃面板，
// 以前宽高写死在 main.go 里（720×480）。屏幕大小和用途差别太大——1080p 上用 720 宽
// 挺合适，4K 上就显小，光标手写板/竖屏上又太宽——所以放开成配置项。
type Panel struct {
	// Width / Height 是逻辑像素（DIP）。0 或没配置表示用默认值。
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// X / Y 是面板左上角的屏幕坐标（物理像素）。拖动面板时默默记下来，
	// 下次启动照这个位置摆。用指针是为了区分"从没记过"（nil，按默认居中偏上）
	// 和"恰好记在 0,0"（左上角屏，那是真实位置）。
	X *int `json:"x,omitempty"`
	Y *int `json:"y,omitempty"`
}

// PanelPos 返回记过的面板位置；从没拖动过时 ok 为 false。
func (c *Config) PanelPos() (x, y int, ok bool) {
	if c.Panel.X == nil || c.Panel.Y == nil {
		return 0, 0, false
	}
	return *c.Panel.X, *c.Panel.Y, true
}

// 面板尺寸的默认值与上下限。下限保证能看见至少一行结果 + 顶栏底栏；
// 上限只是防止填错把面板撑出屏幕（多屏/超大屏用户自己按实际填）。
const (
	DefaultPanelWidth  = 720
	DefaultPanelHeight = 480
	MinPanelWidth      = 420
	MinPanelHeight     = 260
	MaxPanelWidth      = 2400
	MaxPanelHeight     = 1600

	// DefaultSignal 是跨网互传的信令地址：Cloudflare Worker 的自有域名。
	// workers.dev 那个默认域名在大陆会被 SNI 阻断（TCP 能连、TLS 握手就被掐），
	// 所以绑了 1jia.life 下的子域名。换服务时在互传窗口里改，会记进配置。
	DefaultSignal = "https://signal.1jia.life"
)

// PanelSize 返回配置里生效的面板宽高，越界或没配置时用默认值。
func (c *Config) PanelSize() (w, h int) {
	w, h = c.Panel.Width, c.Panel.Height
	if w <= 0 {
		w = DefaultPanelWidth
	}
	if h <= 0 {
		h = DefaultPanelHeight
	}
	w = max(MinPanelWidth, min(w, MaxPanelWidth))
	h = max(MinPanelHeight, min(h, MaxPanelHeight))
	return w, h
}

// Weights 三项权重，内部会归一化，不需要凑成 1。
type Weights struct {
	Frequency float64 `json:"frequency"`
	Type      float64 `json:"type"`
	Accuracy  float64 `json:"accuracy"`
}

// Index 控制索引范围。
type Index struct {
	Roots    []string `json:"roots"`
	Excludes []string `json:"excludes"`
	// Frozen 是"收录但不跟增量"的目录：全量建索引时照常进来（所以搜得到），
	// 但 USN 增量一律不收（见 index.Rule.Freeze）。
	//
	// 这一条专治"自己喂自己"：Baozi 每 5 秒写一次 index.delta / state.json，
	// 每次唤出还会往 baozi.log 追加一行，这些动作全都产生 USN 记录。若把它们
	// 当成"用户动了文件"再索引一遍、再写一条增量，就会 5 秒一轮永不空闲。
	// 反过来把它们整个排除掉，用户又搜不到自己的日志和配置 —— Everything 能搜到
	// 自己的 Everything.db 就是这个道理（它的库很少重写，靠"收录但不追"）。
	//
	// 代价是这些条目的修改时间停在上一次全量重建那一刻，可能偏旧。
	Frozen []string `json:"frozen"`
	// IncludeHidden 只对隐藏**文件**生效；隐藏**目录**一律递归进去，
	// 否则被工具或用户随手设了 Hidden 的备份目录会整棵搜不到。
	IncludeHidden bool `json:"includeHidden"`
	// RescanMinutes 全量重建索引的间隔；0 表示只在启动时建一次。
	RescanMinutes int `json:"rescanMinutes"`
}

// History 控制使用历史（频次与最近使用）。
type History struct {
	Enabled  bool `json:"enabled"`
	MaxItems int  `json:"maxItems"`
}

// Command 是 Listary 式的自定义命令：输入激活词（空格后跟参数），回车执行。
//
// 参数里的占位符在运行时替换（见 cmd/baozi 的 expandArgs）：
//
//	%1 … %9   查询里空格分出的第 1～9 个词
//	%*        全部词
//	{dir}     当前跟随到的文件管理器目录
//
// 一个占位符都没写时走老行为：查询里多出的词按顺序追加到 Args 末尾。
type Command struct {
	// Keyword 是主激活词；Aliases 是同一命令的其它叫法。
	// 搜索时命中任意一个都算（"关机" / "shut" / "shutdown" 指向同一个脚本）。
	Keyword string   `json:"keyword"`
	Aliases []string `json:"aliases,omitempty"`
	Name    string   `json:"name"`
	Exec    string   `json:"exec"`
	Args    []string `json:"args"`
	// Dir 是工作目录，空则继承 Baozi 自己的。同样支持 {dir}。
	Dir string `json:"dir,omitempty"`
	// KeepOpen 执行后不隐藏面板（例如打开一个命令行窗口）。
	KeepOpen bool `json:"keepOpen"`
}

// Keywords 返回这条命令的全部激活词（主词在前，别名按顺序），去空去重。
func (c Command) Keywords() []string {
	out := make([]string, 0, 1+len(c.Aliases))
	seen := make(map[string]bool, 1+len(c.Aliases))
	for _, k := range append([]string{c.Keyword}, c.Aliases...) {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		low := strings.ToLower(k)
		if seen[low] {
			continue
		}
		seen[low] = true
		out = append(out, k)
	}
	return out
}

// Default 给出一份能直接跑的配置：索引所有固定磁盘，排除系统噪声目录。
func Default() *Config {
	// 外观三项的缺省值写成具体数，让 config.json 里一眼能看到"现在是多少"。
	radius, opacity, shadow := DefaultPanelRadius, DefaultGlassTransparency, DefaultShadowPct
	return &Config{
		Appearance: Appearance{
			Radius:  Toggle{Value: &radius},
			Opacity: Toggle{Value: &opacity},
			Shadow:  Toggle{Value: &shadow},
		},
		Hotkey: Hotkey{
			DoubleCtrl:           true,
			DoubleCtrlIntervalMs: DefaultDoubleCtrlIntervalMs,
			Combo:                "Alt+Shift+Space",
			DisableInFullscreen:  true,
		},
		Search: Search{
			Weights: Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2},
			TypeOrder: []string{
				".exe", ".lnk", ".msc", ".bat", ".cmd", ".ps1",
				".url", ".appref-ms", ".ps1xml", ".com", ".jar",
			},
			MaxResults: 80,
		},
		Index: Index{
			Roots:         []string{},
			Excludes:      defaultExcludes(),
			Frozen:        defaultFrozen(),
			IncludeHidden: false,
			RescanMinutes: 0,
		},
		History: History{Enabled: true, MaxItems: 200},
		Share:   Share{Bind: "", TTLMinutes: 60}, // 默认所有网卡、链接活 1 小时
		Commands: []Command{
			{Keyword: "term", Name: "Windows Terminal", Exec: "wt.exe"},
			{Keyword: "code", Name: "VS Code", Exec: "code"},
			{Keyword: "explore", Name: "打开资源管理器", Exec: "explorer.exe"},
		},
	}
}

// Dir 返回配置与数据目录（%LOCALAPPDATA%\Baozi）。
// BAOZI_DATA_DIR 可覆盖，用于隔离测试或多份互不干扰的配置。
func Dir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BAOZI_DATA_DIR")); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
		return d, nil
	}
	base, err := os.UserCacheDir() // Windows: %LOCALAPPDATA%
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "Baozi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Path 返回 config.json 的完整路径。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load 读取配置；文件不存在时落盘一份默认配置，保证用户能直接编辑。
func Load() (*Config, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		cfg.Index.Roots = SystemRoots()
		if saveErr := Save(cfg, path); saveErr != nil {
			return nil, path, fmt.Errorf("写入默认配置: %w", saveErr)
		}
		return cfg, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	cfg := Default()
	cfg.Index.Roots = SystemRoots()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, path, fmt.Errorf("解析 %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, path, nil
}

// Save 原子写入配置。
//
// 覆盖之前先把上一版留成 config.json.bak：这份文件里装着用户的命令、热键、
// 索引范围，一旦被一次"状态不对"的保存覆盖掉（比如界面还没读完就点了保存），
// 没有任何东西能找回来。备份失败不影响保存本身 —— 存下去比留底重要。
func Save(cfg *Config, path string) error {
	if cfg.Index.Roots == nil {
		cfg.Index.Roots = SystemRoots()
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) normalize() {
	if c.Hotkey.DoubleCtrlIntervalMs <= 0 {
		c.Hotkey.DoubleCtrlIntervalMs = DefaultDoubleCtrlIntervalMs
	}
	if c.Search.MaxResults <= 0 {
		c.Search.MaxResults = 80
	}
	if len(c.Search.TypeOrder) == 0 {
		c.Search.TypeOrder = Default().Search.TypeOrder
	}
	if c.Search.Weights == (Weights{}) {
		c.Search.Weights = Default().Search.Weights
	}
	if c.History.MaxItems <= 0 {
		c.History.MaxItems = 200
	}
	if c.Share.TTLMinutes <= 0 {
		// 老配置里没有这一项：默认 1 小时。
		c.Share.TTLMinutes = 60
	}
	if len(c.Index.Roots) == 0 {
		c.Index.Roots = SystemRoots()
	}
	// 把"一定不该索引"的缓存目录并进用户列表：老配置里没有这些项，
	// 不合并的话隐藏目录解禁后 AppData 里几万个缓存文件会立刻进来抢结果。
	c.Index.Excludes = mergeExcludes(defaultExcludes(), c.Index.Excludes)
	// 自用目录（配置/日志/索引文件）走"收录但不跟增量"：老配置里没有这一项，
	// 得补上，否则第一次增量就把自己的写入喂成永动机。
	c.Index.Frozen = mergeExcludes(defaultFrozen(), c.Index.Frozen)
	// 上一版把自用目录写进"排除"里存盘了，读回来和冻结冲突。两者互斥时冻结优先：
	// 排除了就搜不到用户自己的 config.json / baozi.log，而冻结本来就已经把
	// 自己的写盘挡在增量之外（见 index.Rule.Freeze）。
	c.Index.Excludes = subtractRules(c.Index.Excludes, c.Index.Frozen)
	c.History.Enabled = true // 目前历史是排序权重的一部分，恒开
}

// defaultExcludes 是永远不该进索引的目录：回收站、系统维护目录、依赖目录，
// 以及 AppData 里的各类缓存/编译产物。用户可以在自己的列表里追加，但删不掉这些。
func defaultExcludes() []string {
	return []string{
		"$Recycle.Bin", "System Volume Information", "$WinREAgent",
		"node_modules", ".git", ".svn", ".hg", ".cache",
		"Windows\\WinSxS", "Windows\\servicing", "Windows\\component store",
		"AppData\\Local\\Temp", "AppData\\Local\\Packages",
		// 缓存与编译产物：双击这些文件没有意义，只会把真正的文件挤出结果。
		"AppData\\Local\\go-build", "AppData\\Local\\pnpm", "AppData\\Local\\npm-cache",
		"AppData\\Local\\pip", "AppData\\Local\\python", "AppData\\Local\\Thorium",
		"AppData\\Local\\doubaowork", "AppData\\Local\\Microsoft\\Windows\\Explorer",
		"AppData\\Local\\Microsoft\\Windows\\INetCache",
		// "最近使用"的快捷方式集合：Baozi 自己就记历史，索引它只是把 .lnk 塞满结果。
		"AppData\\Roaming\\Microsoft\\Windows\\Recent",
		"AppData\\Roaming\\Tencent",
	}
}

// defaultFrozen 是"收录但不跟增量"的自用目录（见 Index.Frozen 的说明）。
//
// 这里**不能**写成排除：用户想搜到自己刚改的 config.json、想看 baozi.log，
// 都是再正常不过的事。所以它进全量索引、能搜到；只是 USN 增量对它一律不收 ——
// 它自己 5 秒一次的写盘就不会反过来触发索引，也就不用担心自喂。
func defaultFrozen() []string {
	return []string{
		"AppData\\Local\\Baozi",
	}
}

// mergeExcludes 让 base 始终生效，再补上用户自己的规则（大小写不敏感去重）。
func mergeExcludes(base, user []string) []string {
	seen := make(map[string]bool, len(base)+len(user))
	out := make([]string, 0, len(base)+len(user))
	for _, list := range [][]string{base, user} {
		for _, rule := range list {
			rule = strings.TrimSpace(rule)
			if rule == "" {
				continue
			}
			key := strings.ToLower(rule)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, rule)
		}
	}
	return out
}

// subtractRules 从 list 里去掉和 drop 完全相同的规则（大小写不敏感）。
// 只比全等，不做前缀/子串推算：用户手写的 `AppData\Local\Baozi\logs` 那种更细的
// 排除应当保留 —— 冻结只负责整个目录"收录但不跟增量"这件粗活。
func subtractRules(list, drop []string) []string {
	if len(drop) == 0 {
		return list
	}
	gone := make(map[string]bool, len(drop))
	for _, rule := range drop {
		gone[strings.ToLower(strings.TrimSpace(rule))] = true
	}
	out := list[:0]
	for _, rule := range list {
		if gone[strings.ToLower(strings.TrimSpace(rule))] {
			continue
		}
		out = append(out, rule)
	}
	return out
}

// SystemRoots 探测本机固定磁盘盘符。
func SystemRoots() []string {
	var roots []string
	for _, r := range "CDEFGHIJKLMNOPQRSTUVWXYZ" {
		root := string(r) + `:\`
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			roots = append(roots, root)
		}
	}
	return roots
}

// Quote 供帮助文案使用。
func Quote(s string) string { return strings.TrimSpace(s) }
