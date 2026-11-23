package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 图标尺寸的缺省语义：没配置就跟着系统缩放，配了就照配置（不再乘缩放）。
// 面板尺寸：没配置走默认 720×480；填了就照填的来，越界夹到上下限。
// 夹取的目的是"填错也别把面板撑出屏幕或缩成一条缝"。
func TestPanelSize(t *testing.T) {
	var c Config
	if w, h := c.PanelSize(); w != DefaultPanelWidth || h != DefaultPanelHeight {
		t.Fatalf("默认应为 %d×%d，实际 %d×%d", DefaultPanelWidth, DefaultPanelHeight, w, h)
	}

	c.Panel.Width, c.Panel.Height = 1200, 900
	if w, h := c.PanelSize(); w != 1200 || h != 900 {
		t.Fatalf("填了 1200×900 应原样生效，实际 %d×%d", w, h)
	}

	c.Panel.Width, c.Panel.Height = 100, 100
	if w, h := c.PanelSize(); w != MinPanelWidth || h != MinPanelHeight {
		t.Fatalf("过小应夹到 %d×%d，实际 %d×%d", MinPanelWidth, MinPanelHeight, w, h)
	}

	c.Panel.Width, c.Panel.Height = 9999, 9999
	if w, h := c.PanelSize(); w != MaxPanelWidth || h != MaxPanelHeight {
		t.Fatalf("过大应夹到 %d×%d，实际 %d×%d", MaxPanelWidth, MaxPanelHeight, w, h)
	}
}

func TestIconSizeAt(t *testing.T) {
	var c Config
	if got := c.IconSizeAt(1); got != BaseIconSize {
		t.Fatalf("100%% 缩放应为 %d，实际 %v", BaseIconSize, got)
	}
	if got := c.IconSizeAt(1.25); got != BaseIconSize*1.25 {
		t.Fatalf("125%% 缩放应为 %v，实际 %v", BaseIconSize*1.25, got)
	}
	// scale 拿不到（0）不能算出 0 —— 那样图标会整个消失。
	if got := c.IconSizeAt(0); got != BaseIconSize {
		t.Fatalf("缩放未知时应退回 %d，实际 %v", BaseIconSize, got)
	}

	c.Search.IconSize = 34
	if got := c.IconSizeAt(1.25); got != 34 {
		t.Fatalf("用户填了 34 就该是 34（不再乘缩放），实际 %v", got)
	}

	// 倍数（新配置）优先于旧的像素字段：1 倍就是系统缩放下的默认大小。
	c.Search.IconSize = 0
	c.Search.IconScale = 1
	if got := c.IconSizeAt(1.25); got != BaseIconSize*1.25 {
		t.Fatalf("100%% 倍应是默认大小 %v，实际 %v", BaseIconSize*1.25, got)
	}
	// 125% 是"在默认基础上再大一圈"，系统缩放照样要乘进去。
	c.Search.IconScale = 1.25
	if got := c.IconSizeAt(1.25); got != BaseIconSize*1.25*1.25 {
		t.Fatalf("125%% 倍应再放大一圈，实际 %v", got)
	}
	// 手输的离谱值要被夹住：0.1 倍会小到认不出，10 倍会把格子撑爆。
	for _, tc := range []struct {
		in, want float64
	}{{0.1, MinIconScale}, {10, MaxIconScale}} {
		c.Search.IconScale = tc.in
		if got := c.IconSizeAt(1); got != float32(BaseIconSize*tc.want) {
			t.Fatalf("%v 倍应夹到 %v，实际 %v", tc.in, tc.want, got)
		}
	}
}

// 目录来源开关的缺省语义：配置里没有这一块 → 全部启用（老配置的行为不变）。
func TestPathSourcesDefaultOn(t *testing.T) {
	var c Config
	for _, kind := range []string{"explorer", "totalcmd", "opus", "xyplorer", "xplorer2", "tessoa", "arke"} {
		if !c.PathSourceEnabled(kind) {
			t.Fatalf("%s 在缺省配置下应当是启用的", kind)
		}
	}
}

// 写了明确 false 的来源要真的关掉，写了 true 的要开；没写的仍按"缺省即开"。
func TestPathSourcesExplicitFalse(t *testing.T) {
	off := false
	on := true
	c := Config{PathSources: &PathSources{
		Opus:     &off,
		Xyplorer: &off,
		Xplorer2: &on,
	}}
	cases := map[string]bool{
		"explorer": true,  // 没写
		"totalcmd": true,  // 没写
		"opus":     false, // 明确关
		"xyplorer": false, // 明确关
		"xplorer2": true,  // 明确开
	}
	for kind, want := range cases {
		if got := c.PathSourceEnabled(kind); got != want {
			t.Fatalf("%s: 期望 %v，实际 %v", kind, want, got)
		}
	}
}

// 未知来源（拼错的 kind）不能因为认不出来就一律关掉 —— 当作启用，行为最接近"没影响"。
func TestPathSourcesUnknownKindOn(t *testing.T) {
	off := false
	c := Config{PathSources: &PathSources{Explorer: &off}}
	if !c.PathSourceEnabled("files") {
		t.Fatal("未知来源应当按启用处理")
	}
}

// 设置页写进去的 JSON 必须是设置页读得出来的形状（键名与 win.Source* 一致）。
func TestPathSourcesJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAOZI_DATA_DIR", dir)

	off := false
	on := true
	follow := false
	c := Default()
	c.DialogFollow = &follow
	c.PathSources = &PathSources{Explorer: &on, TotalCmd: &on, Opus: &off, Xyplorer: &on, Xplorer2: &off, Arke: &off}

	path := filepath.Join(dir, "config.json")
	if err := Save(c, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, gotPath, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotPath != path {
		t.Fatalf("配置路径: 期望 %s，实际 %s", path, gotPath)
	}
	if got.DialogFollowOn() {
		t.Fatal("dialogFollow=false 应当读回关闭")
	}
	if got.PathSourceEnabled("opus") {
		t.Fatal("opus=false 应当读回关闭")
	}
	if !got.PathSourceEnabled("xyplorer") {
		t.Fatal("xyplorer=true 应当读回开启")
	}
	if got.PathSourceEnabled("arke") {
		t.Fatal("arke=false 应当读回关闭")
	}

	// 落盘的键名要和跟随器 / 设置页用的字面量一致，否则改名之后静默失效。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 config.json: %v", err)
	}
	for _, key := range []string{`"explorer"`, `"totalcmd"`, `"opus"`, `"xyplorer"`, `"xplorer2"`, `"arke"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("pathSources 缺少键 %s：%s", key, data)
		}
	}
}

// 面板位置：从没拖动过时读不出东西（调用方据此走"居中偏上"），
// 记过一次之后要能原样读回、并且落盘后还在。
// 用指针字段就是为这个：0,0 是左上角屏的真实位置，不能和"没记过"混为一谈。
func TestPanelPos(t *testing.T) {
	var c Config
	if _, _, ok := c.PanelPos(); ok {
		t.Fatal("空白配置不该有面板位置")
	}

	x, y := 1180, 260
	c.Panel.X, c.Panel.Y = &x, &y
	if gx, gy, ok := c.PanelPos(); !ok || gx != x || gy != y {
		t.Fatalf("应读回 %d,%d，实际 %d,%d ok=%v", x, y, gx, gy, ok)
	}

	// 0,0 必须算"记过"。
	zero := 0
	c.Panel.X, c.Panel.Y = &zero, &zero
	if gx, gy, ok := c.PanelPos(); !ok || gx != 0 || gy != 0 {
		t.Fatalf("0,0 应读回 0,0 ok=true，实际 %d,%d ok=%v", gx, gy, ok)
	}
}

// 自用数据目录不能进"排除"，只能进"冻结"：
// 排除就搜不到自己的 config.json / baozi.log，不冻结又会把自己的写盘喂成增量永动机。
func TestDataDirIsFrozenNotExcluded(t *testing.T) {
	c := Default()
	c.normalize()
	for _, rule := range c.Index.Excludes {
		if strings.EqualFold(rule, `AppData\Local\Baozi`) {
			t.Fatalf("排除规则里不该再有自用目录：%v", c.Index.Excludes)
		}
	}
	frozen := false
	for _, rule := range c.Index.Frozen {
		if strings.EqualFold(rule, `AppData\Local\Baozi`) {
			frozen = true
		}
	}
	if !frozen {
		t.Fatalf("冻结规则里应当有自用目录：%v", c.Index.Frozen)
	}
}

// 老配置里没有 frozen 这一项：读回来必须补上，否则第一次增量就把自己喂成永动机。
func TestFrozenMergedFromOldConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAOZI_DATA_DIR", dir)
	path := filepath.Join(dir, "config.json")
	old := `{"index":{"roots":["C:\\"],"excludes":[]}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("写旧配置: %v", err)
	}
	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hasSelf := false
	for _, rule := range cfg.Index.Frozen {
		if strings.EqualFold(rule, `AppData\Local\Baozi`) {
			hasSelf = true
		}
	}
	if !hasSelf {
		t.Fatalf("老配置读回后没有补上冻结规则：%v", cfg.Index.Frozen)
	}
}

// 上一版把自用目录写进"排除"存盘了：读回来两条规则打架，必须冻结赢 ——
// 留在排除里就搜不到用户自己的 config.json / baozi.log（真实踩过的坑：
// 改成冻结之后索引重建了一轮，面板里还是搜不到日志，原因就是盘上那条旧排除）。
func TestFrozenWinsOverStaleExclude(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAOZI_DATA_DIR", dir)
	path := filepath.Join(dir, "config.json")
	old := `{"index":{"roots":["C:\\"],"excludes":["AppData\\Local\\Baozi","node_modules"]}}`
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("写旧配置: %v", err)
	}
	cfg, _, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, rule := range cfg.Index.Excludes {
		if strings.EqualFold(rule, `AppData\Local\Baozi`) {
			t.Fatalf("旧的排除规则还留着，自用目录就搜不到：%v", cfg.Index.Excludes)
		}
	}
	// 用户自己写的别的排除不能被误删。
	kept := false
	for _, rule := range cfg.Index.Excludes {
		if rule == "node_modules" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("正常的排除规则被删掉了：%v", cfg.Index.Excludes)
	}
}
