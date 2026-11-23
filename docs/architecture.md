# Baozi 架构与取舍

本文写给维护者：机制、公式、边界、已知问题。用户操作看 `README.md`。

## 1. 进程模型

```
baozi.exe           用户态交互层（对应 Lertaro.App）
 ├─ mygo 原生 UI     浮窗 / 托盘 / 设置
 ├─ 索引             读 index.bin 快照（服务模式）；没有服务时自己 WalkDir（见 §9）
 └─ 子进程 baozi-hook.exe   低级键盘钩子 + 全屏判定（对应 Lertaro.Service --hook）
      └─ stdout 一行事件 → 主进程读 → toggle

baozi-index.exe     可选：Windows 服务（LocalSystem），读 NTFS USN Journal 建索引
 └─ 写 index.bin / index.progress.json / index.heartbeat，主进程只读不写
```

`baozi-index.exe` 不在跑的时候，一切行为与之前完全一致（普通权限、目录扫描）。

为什么钩子必须独立进程：

- `WH_KEYBOARD_LL` 需要一个独立的 Windows 消息循环，和 UI 主线程共用会互相拖慢；
- 钩子被反作弊 / 安全软件干掉时，主进程还能活，重启钩子即可；
- 主进程退出时关闭子进程 stdin，钩子读到 EOF 自杀，不留孤儿（已实测）。

**不抄的地方**：Lertaro 把钩子做成 `LocalSystem` 服务拉起的提权进程，用来向管理员权限窗口发消息（UIPI 突破）。
Baozi 暂时不做提权钩子：双击 Ctrl 只需要读按键，普通权限就够；真正需要操作高权限窗口的是第 6 条（文件对话框跳转），
到那一步再按需把 hook 以 `runas` 启动，而不是无条件提权。

## 2. 唤出链路与全屏抑制

双击 Ctrl 的状态机（`cmd/baozi-hook/main.go`）：

```
Ctrl↓ Ctrl↑                    → armed = true，记录 lastCtrlUp
Ctrl↓ 且 now-lastCtrlUp ≤ 200ms → 触发 toggle，armed = false
中间出现任何非修饰键按下         → armed = false（避免 Ctrl+C 被误判）
```

全屏抑制（`internal/win/fullscreen.go`），命中任一即不唤出：

1. `SHQueryUserNotificationState()` 返回 `QUNS_BUSY(2)` / `QUNS_RUNNING_D3D_FULL_SCREEN(3)` / `QUNS_PRESENTATION_MODE(4)`
   —— 覆盖 D3D 独占游戏与演示模式；
2. 前台窗口矩形覆盖所在显示器（容差 2px），且 `GWL_STYLE` 没有 `WS_CAPTION` —— 覆盖无边框全屏的播放器/游戏；
3. 前台窗口是 `Progman` / `WorkerW` / `Shell_TrayWnd` 时永远不算全屏（那是桌面本身）。

主进程 `show()` 里会再判一次，所以走 `RegisterHotKey` 的备用组合键也享受同样的抑制。

为什么快捷键会"唤不出"（Lertaro 的老毛病）：
`RegisterHotKey` 只能注册组合键，被别的程序抢占时注册失败且无提示，高权限窗口或独占全屏下也常不触发。
Baozi 主路径不依赖它，组合键只是兜底。

## 3. 索引

### 数据结构

照 NTFS `$MFT` 的存法：不存完整路径，只存"名字在 arena 里的位置 + 父节点编号"。

```go
// Node 定长 20 字节
type Node struct {
    Parent  int32  // 父节点下标，-1 表示根
    NameOff uint32 // 名字在 Table.Names 里的偏移
    SizeKB  int32  // 文件大小（KB）；目录为 0
    ModSec  int32  // 相对 2020-01-01 的秒数
    NameLen uint16 // 名字字节数
    Kind    Kind
}

type Table struct {
    Nodes []Node
    Names []byte           // arena：所有文件名首尾相接
    Py    map[int32]uint64 // 只对含汉字的条目有值，打包了"偏移+长度"
}
```

取路径时顺着父链往上拼（`Table.PathOf`），代价 O(深度)。

**为什么名字要进 arena 而不是每条一个 `string`**：179 万条如果各自分配，
40.6 MB 的实际内容会占到 ~90 MB（16 字节 string 头、分配器按 size class 向上取整、
每个对象的管理开销），还附带上百万个对象让 GC 每次都得扫。进 arena 之后是连续一块，
对象数从 209 万降到 4.9 万。

拼音首字母串同理：只有 9 千条含汉字的名字需要，旧结构却给每条都留了一个 16 字节字符串头
（约 28.6 MB 纯浪费）。现在只给这 9 千条在 arena 尾部留位置，`Py` 里存打包的偏移+长度。

### 扫描

两条数据源，产出同一张 `Table`（同一个 `Builder`，同一套排除规则）：

- **服务模式**：`internal/usn.BuildAll()` 读 `$MFT` 记录还原路径（16 秒 / 174 万条，见 §9）；
- **进程内模式**：`index.Build()` 按配置根目录 `filepath.WalkDir`，规则：

- 排除规则不带分隔符 → 按目录名/文件名匹配（支持通配）；带 `\` → 按路径片段匹配；
- `defaultExcludes()` 那几条**永远生效**，`normalize()` 把用户的列表 `mergeExcludes()` 上去
  （大小写不敏感去重）。原因是老配置里不可能有新增项，而隐藏目录解禁后 AppData 下几万个
  缓存文件会立刻涌进结果：实测解禁前后索引从 141 万涨到 192 万，其中 AppData 占 18.7 万，
  且 `Recent\*.lnk` 会靠类型分（0.85）把真正的文件挤出前列；
- 不跟随 reparse point（符号链接、目录联接），避免环路和重复索引；
- `includeHidden=false` **只**跳过带 `FILE_ATTRIBUTE_HIDDEN` 的**文件**；隐藏**目录**照常递归。
  这条是被真实案例逼出来的：`D:\Backup` 带 Hidden 属性，旧逻辑整棵 `SkipDir`，
  用户备份盘里几十个文件一条都没进索引，表现为"明明有这个文件却搜不到"；
- 单个条目权限不足只跳过该条目，不中断整盘；
- 根目录去重并去掉互相嵌套的项（`C:\` 和 `C:\Users` 同时配置时只扫前者）。

### 落盘

`index.bin`（v4，定长记录 + arena，**未压缩**），不再是 gob、也不再 gzip。格式：

```
0..40  固定头：magic "BZID" | ver | count | namesLen | rawLen | rootsLen | pyCount | nodesOff | namesOff | pyOff
40..   roots JSON
..     [补齐到 8 字节]
..     nodes  count × 20 字节
..     names  namesLen（所有文件名首尾相接）
..     py     pyCount × 10 字节（索引 u32 + 偏移 u32 + 长度 u16）
```

加载时 **整份文件 MapViewOfFile 映射**，`nodes` 段 `unsafe.Slice` 成 `[]Node`、`names` 段直接切片——
零解码、零拷贝，而且这些页是 **file-backed 的干净页**：内存紧的时候系统可以直接丢掉，
下次访问再从文件读回来。堆里只留一个表头（几十字节）和 `Py` 那个九千条的小 map。

这就是 v3 → v4 的全部意义。v3 虽然也"零拷贝"，但那块 55 MB 的缓冲是解压到 Go 堆上的匿名内存：
全量常驻、页还回不去，再加上 `Clone` 让每一代快照都引用它，于是三份表同时驻留（pprof 实测
`Load` 73.76 + `ApplyOps` 42.09 + `Clone` 41.02）。v4 之后同样的场景堆占用从 86 MB 掉到 **8 MB**。

磁盘代价：19 MB（gzip）→ 74 MB（未压缩）。这笔账是划算的——索引文件躺在本地盘上，
省下来的是**每次唤出都在的常驻内存**；Private 从 251 MB 降到 173 MB 就是它。

一次映射两个坑：

- **文件被映射后不能替换/删除**。服务端每轮重建完要 `os.Rename(tmp, index.bin)`，而主进程正映射着
  上一代 → `Access is denied`。对策是"代次文件"：写不进去就写 `index-<时间戳>.bin`，
  两边都按目录里的 mtime 挑最新那份（`index.Latest`），加载完顺手 `index.Prune` 收掉更老的。
  删不掉的（正被自己映射着）留到下次启动再删。
- **映射的生命周期跟着 Table 走**。`Clone` 出来的表共享同一份映射，所以 `mapping` 带引用计数
  （`retain/release`），最后一个 Table 被 GC 回收时才 `UnmapViewOfFile` + `CloseHandle`。
  不能在"换表"那一刻直接关：搜索线程可能还拿着旧快照在遍历。
- 版本不匹配（比如现存的是 v3 gzip 文件）直接判为需要重建。

为什么换掉 gob、又为什么从 gzip 换成未压缩，见第 15 节（都跟内存有关）。

### 增量层：基座只读 + overlay

从 v4 起 `Table` 是两层：

```
基座   Nodes/Names/Py  指向映射区，只读。绝不 append、绝不改元素。
增量   ovNodes/ovNames/ovPy  堆上的小数组，按 USN 顺序追加
       dead                  墓碑集合：下标 → 已删除
```

改动前增量走的是 copy-on-write：`Clone()` 复制一份**可写**的表（179 万条 = 76 MB 复制 ≈ 15 ms），
然后原地改。30 秒一次的增量让 WS 反复多挂 100 MB。现在 `Clone()` 只复制增量层那个尾巴
（几十 KB ~ 几 MB），基座共享——代价从"复制整表"变成"追加一条"。

墓碑为什么是集合而不是改 `Kind`：基座是共享的只读映射页，改一个字节就把整个 4 KB 页弄脏了。
`Table.KindOf/Dead` 会先查这个集合再上溯父链，调用方不必自己拼。

增量攒够了（墓碑 2 万条 或 名字 arena 追加 8 MB）才全量重建，见第 11 节。

### 搜索预筛：不再预存小写表

旧实现给每条名字预存一份 `strings.ToLower` 的结果（`catalog.lowers`，实测 39 MB + 42 万个对象）。
现在直接在 arena 字节上做大小写不敏感的子串比较（`Table.Hits`）：ASCII 折大小写，
汉字的 UTF-8 字节都 >0x7f、原样比较才是对的。零分配，也不用为 179 万条多存一份。

搜索主循环因此从"读预生成的小写串"变成"读 arena 的一段字节"，
命中的少数候选才取 `Name(i)` 构造 string（会分配一次，但候选最多 5000 条）。

## 4. 排序（可直接复算）

排序是**两段式**：先定段（硬边界），段内才算分。

### 4.1 段

| 段 | 条件（`search.BandOf`） |
|---|---|
| 0 应用 | `Kind == KindApp`——不管匹配多弱，都在最前 |
| 1 命令 | `Kind == KindCommand` |
| 2 强匹配 | `Acc >= 0.88`（全等 / 前缀 / 词边界 / 拼音全等） |
| 3 中匹配 | `Acc >= 0.62`（名字或路径里含查询）；该路径 `Opens >= 3` 时抬到段 2 |
| 4 弱匹配 | 其余（散落子序列） |

段号小的永远在前，段内才比 Score。**跨段永不越级**——这是"输 `qq` 先看到 QQ"的结构性保证，
不靠权重平衡。

### 4.2 段内分数

```
Freq  = 0.7 * log1p(count)/log1p(10) + 0.3 * exp(-ageHours/(24*30))
Type  = 开始菜单应用 1.00；自定义命令 0.95；目录 0.60
        类型优先级第 i 名 → 0.90 - 0.05*i（下限 0.15）；未列入类型 0.20
Acc   = 见下表
Score = (0.5*Freq + 0.3*Type + 0.2*Acc) / (0.5+0.3+0.2)
```

| 匹配形态 | Acc |
|---|---|
| 文件名完全等于查询 | 1.00 |
| 拼音首字母串完全等于查询（`dbgz` → 豆包工作） | 0.97 |
| 文件名以查询开头 | 0.95 |
| 拼音全拼完全等于查询（`doubaogongzuo`） | 0.95 |
| 拼音首字母串以查询开头（`dbg`） | 0.90 |
| 命中词边界（空格 `.` `-` `_` `(` `[` `\` `/` `@` `+` `,` `{` 之后） | 0.88 |
| 拼音全拼以查询开头 | 0.88 |
| 文件名包含查询 | 0.78 |
| 拼音全拼包含查询 | 0.70 |
| 目录路径包含查询 | 0.62（只在查询里带 `\` 或 `/` 时生效；应用不传路径，见下） |
| 字符按顺序散落（`vsc` → Visual Studio Code） | 0.35–0.52（按连续度） |
| 拼音首字母散落命中 | 0.45 |
| 多关键词 | 取各 token 最小值（AND 语义）；拼音只在单关键词时启用 |

频次是**绝对刻度**：打开 10 次算满分，开 1 次只有 0.29。以前是
`log1p(count)/log1p(maxCount)`，分母取"历史里最多的那条"，而 `maxItems=200` 里最常用的
往往只有三五次——于是**打开过一次就有 0.63**，配上 0.5 权重就是 0.31 分，
一条随手开过的 `qq-log.txt` 能压过名字全等、从没打开过的应用。半衰期从 7 天改成 30 天：
7 天太短，前天开的文件今天就掉没了，反而让临时文件在那一两天里霸榜。

**为什么主序是匹配质量，而不是扩展名**（改过一轮）：先按扩展名分段（应用/命令/类型顺序/目录/普通文件），
实测搜 `code` 时前 8 条全是 `C:\Program Files` 下的 `wsdcodegen.exe`、`frcode.exe`、`pdfdecode.exe`
——它们是"名字包含 code"（Acc 0.78），但 `.exe` 的类型分 0.90 对普通文件 0.20，
跨段比较永远是它们赢。扩展名最能表达"用户想要什么"，却最不能表达"这条到底像不像我在找的东西"。
换成按 Acc 分段后，同一台机器同一次查询：前 10 条全部是 Acc 1.00/0.95 的强匹配，
`wsdcodegen.exe` 落到中段。

类型顺序的最高档只到 **0.90**，不是 1.00：若 `.exe` 也给 1.00，它会和应用同分，
胜负只能落到准确度上，应用就沉下去了。应用、命令不吃 `typeOrder`，档位写死在 `search.TypeScore`。

### 4.3 噪声

后缀黑名单（`search.noisyExts`）：`.log .tmp .bak .old .dll .sys .ini .db .cache .pid
.etl .dmp .pdb .ilk .obj .class .pyc .lock .swp .swo .part .crdownload .download .nfo .torrent`

路径片段黑名单（`search.noisyPathFrags`）：`\Windows\`、`\AppData\`、`\ProgramData\`、`\Packages\`、
`\Temp\`、`\node_modules\`、`\Program Files\Common Files\`、`$Recycle.Bin`、`windows.old`、
`.git`、`.svn`、`.cache`、`\cache\`、`.bun`、`site-packages`、`vendor`、`\obj\`、
`.nuget`、`go-build`、`.gradle`、`dist-info`

命中后：**降一档**（段号 +1）且 `Score × 0.6`，但**不隐藏**——用户真要找 `.log` 还找得到。

`\cache\`、`.bun`、`vendor` 这几条是实测补的：第三方 CLI 的 `.exe` 常常埋在
`.bun\install\cache\@openai\codex@...\vendor\` 里，靠 `.exe` 的类型分排到最前。

刻意**不含 `\Program Files\`**：那里装的是用户真正会用的程序（git、notepad++、GuoheView…），
整片拉黑会让"搜 `git` 找我装的 git"变难；靠匹配档位已经足够把它们的辅助程序压下去。

### 4.4 同分比较

段 → 总分 → Acc → Freq → 名字更短 → 字典序（`search.Less`）。

用户要的"第一频次、第二类型、最后准确性"仍然是段内的默认权重 0.5 / 0.3 / 0.2；
权重可改，改完不用重启（下次打开配置时生效于新查询）。

**应用的 Path 不参与匹配**：开始菜单应用的 Path 是 `shell:AppsFolder\<AUMID>` 或磁盘路径，
而 0.62 这一档是"整条路径包含查询"，会把路径中间任意子串都算命中 —— 实测搜 `cal` 时，
`C:\Users\…\AppData\Local\nvm\nvm.exe` 里的 `Local` 含 `cal`（Lo-cal），于是 nvm、Python 3.11/3.12/3.14
全冒出来（还带着应用类型权重排在最前），真正的计算器反被挤到第四。
现在应用只走两路：显示名（含拼音）+ `search.AppAlias(ID)` 抽出来的产品名
（`Microsoft.WindowsCalculator_8wekyb3d8bbwe!App` → `Microsoft WindowsCalculator`，
`…\nvm\nvm.exe` → `nvm`），取分高的那个；`{GUID}` 这种自动生成的 AUMID 直接返回空，不参与匹配。
改完搜 `cal`：只有「计算器」，nvm 与 Python 全部消失。

**别名只认"像"，不认"凑"**：产品名走 `search.AccuracyAlias`，门槛是 0.62（前缀 / 词边界 / 子串），
字符散落的 0.35~0.52 不算命中。别名是英文产品名（`quark cloud drive`），散落子序列几乎只能碰巧：
搜 `arke` 曾命中 `quark cloud drive`（a-r-k…e），而应用段是硬置顶，无关的夸克网盘直接排到第一。
`AppAlias` 同时去掉 AUMID 里重复的段（`WorkBuddy.WorkBuddy` → `WorkBuddy`），否则 `kk` 也能靠两个 `k` 凑进去。

**应用列表不是启动后就定死**：启动时读一次开始菜单；之后每次唤出用 `startMenuModTime`（递归看两个
Start Menu 目录的最新 mtime，几毫秒、两秒内不重复查）比较 `appsMTime`，有变化就在后台重读并在面板
开着时重排结果——新装的软件不用重启 Baozi。纯 UWP（只进 AppsFolder、不落 .lnk）不产生 mtime 变化，
仍要重启才能看到。


### 结果构成：应用优先，但不能只剩应用

段 0 挡不住"应用铺满全榜"：输入单个字母 `d`，191 个开始菜单应用里有几十个命中且全在段 0，
前 80 条会全是应用。所以截断时做配额（`truncateMixed`）：80 条里固定留 `max/4`（默认 20 条）
给非应用，应用最多占 60 条。普通文件类型分只有 0.20，不给名额就永远进不了前 80。

**不再打散应用连续块**：`search.Balance` 已从 `finish()` 摘掉（函数本身保留给测试）。
分段已经把类型排成硬顺序，再交错等于把文件硬塞回应用前面，正好和"输 `qq` 先看到 QQ"相反。
实测 `d`：`结果 80: 应用 60, 非应用 20`，第 61 条起是保底名额捞上来的文件。

### 拼音怎么接进来的

- `internal/pinyin` 用 `mozillazg/go-pinyin`，只保留 `[a-z0-9]` 并统一小写：
  `豆包工作 (Beta)` → `dbgzbeta`，输入不用纠结空格和括号。
- **索引节点存一份首字母串**（`index.Node.Py`），只对含汉字的名字算，纯 ASCII 名字留空。
  这样敲 `dbgz` 时是字符串比较，不用在百万条目上逐条转拼音。
  落盘版本因此从 1 升到 2，旧快照会自动重建一次。
- 预筛也要跟着改：`dbgz` 在文件名里找不到字面，`containsAll` 会把它全滤掉，
  所以加了 `pyContains`——拼音首字母串包含全部 token 就放行。
- 应用（开始菜单，几百个）在加载时同时算首字母串和全拼串，三路都能命中。
- 弱点：多音字取字典默认读音（"重"可能算成 `z` 而不是 `c`）；
  首字母只做"包含"不做散落子序列（`dz` 匹配不到 `dbgz`），因为后者在全表上太贵。

### 正则模式（`internal/search/regex.go` + `searchRegex`）

搜索框右边的 `.*` 按钮把查询从"模糊检索"切成"正则精确匹配"。两条路在
`searchAll` 第一行就分流，正则模式**完全不碰**下面那些东西：拼音串、token 拆词、
词首/子序列打分档位、`byPath` 的路径包含判定。

口径：

- 模式按 Go `regexp`（Perl 语法）编译，**区分大小写**，要忽略就写 `(?i)`。
- 只匹配**文件名**（索引 arena 里那一段字节），不匹配完整路径。
  普通模式靠"查询里有没有 `\` 或 `/`"判断要不要搜路径，但正则里 `\.log$` 这种
  反斜杠到处都是，那个判断在这条路上不成立；顺带省掉了 179 万次 `PathOf`。
- `Acc` 恒为 `1.0`（`search.RegexAccuracy`）：命中就是命中，没有"匹配得好不好"。
  分数退化成 `Freq` + `Type` 两项，也就是用户定的"频次第一"。
- 收集上限 `maxRegexHits = 20000`（普通模式是 5000）：正则的命中面更宽
  （`\.log$` 一项就 8600 多条），上限太小会在排序之前把该靠前的丢掉。
- 每 2048 条回看一眼查询是否还等于发起时的字符串，变了就直接放弃这一遍
  （清空键盘后旧的正则还在扫，会和新的抢 CPU）。
- 编译失败不静默：错误存进 `regexErr`，结果区显示"正则无效：……"。

**为什么必须做字面预筛**：直接 `re.Match(nameBytes)` 全量 174 万条，
`^.*\.log$` 要 647 ms。做法是用 `regexp/syntax` 解析语法树，求出"一定出现的字面"，
先用零分配的子串比较（`containsFoldASCII`）筛一遍：

| 语法节点 | 取字面的规则 |
|---|---|
| `Literal` | 整段字面本身；`(?i)` + 非 ASCII 时**放弃**（Go 会折 ä/Ä，预筛只折 ASCII） |
| `Concat` | 各子式都必须命中 → 多个"单组"字面**合并**成更强的 AND |
| `Alternate`（`\|`） | 每个分支都得给出字面，合并成**或**的组；任一分支求不出就整体放弃 |
| `Plus` / `Repeat{Min>=1}` | 内层必须出现，沿用它 |
| `Star` / `Quest` / `Repeat{Min=0}` | 可能出现 0 次，**放弃** |
| `Capture` | 内层 |

`rpt|backup` 是最好的反例：只取"整条正则里最长的字面"（`backup`）会漏掉
141 条只含 `rpt` 的命中（263 vs 404）。按分支求或组之后，预筛只放行 518 条，
命中 404 条，一条不少。

实测（本机 174 万条，`tools/regexprobe` 跑的完整循环：预筛 + 正则 + 命中后 `PathOf`/`Ext`）：

| 模式 | 命中 | 耗时 |
|---|---|---|
| `^.*\.log$` | 8636 | 67 ms |
| `\d+\.log$` | 6064 | 65 ms |
| `rpt\|backup` | 404 | 110 ms |
| `\.(jpg\|png)$` | 103108（截到 20000） | 52 ms |
| `\d{4}-\d{2}-\d{2}`（无字面，全量扫） | 1486 | 213 ms |

没有字面的模式最贵也就 200 毫秒出头，而且和普通搜索一样跑在
`refilter` 的 goroutine 里，不占 UI 线程。

## 5. 历史

`history.json` 每条是 `{path, name, kind, ext, hint, exec, args, count, lastUsed, pinned, pinOrder}`。

- **元数据**：`kind` 是 `file` / `dir` / `app` / `command`。应用记录的是 `shell:AppsFolder\<AppUserModelID>`，
  只看路径的话 `filepath.Base` 会得到一串 AUMID、按扩展名取图标只会拿到通用文件图标——这就是"名字和图标不对劲"的根因。
  写入时用 `TouchEntry(Entry)` 合并元数据（新值为空不覆盖旧值），读出时优先用记录里的 `name/kind/ext/hint`，
  老记录只有 `path` 才回退推断。图标因此可以按"路径/AUMID"取 `Shell.FileIcon`，而不是按扩展名取 `TypeIcon`。
- **持久化时机**：每次 `Touch` 后 400ms 防抖 `Save()`（`time.AfterFunc` + `Reset`），`Flush()` 供退出前补一刀。
  早先只在 `OnWillQuit` 落盘，进程被强杀/崩溃就全丢——现在不依赖"正常退出"。
- **排序**：`0.7*log1p(count)/log1p(10) + 0.3*exp(-ageHours/720)`（绝对刻度：开 10 次满分；
  30 天半衰）。`maxCount` 字段还在维护，但 `Score` 不再用它——相对化会让"只开过一次"
  拿到 0.63，正好是"文件压过应用"的燃料。`lastUsed` 单调递增（同一毫秒内两次打开也不会乱序）；
  超上限按次数裁剪。空查询时 `Recent(12)` 就是"快捷菜单里的历史"。
- **已固定的位置是显式的**（`pinned` + `pinOrder`）：`pinOrder` 从 1 起，越小越靠左，由首页拖动决定；
  固定时取 `pinSeq+1` 追加到末尾，取消固定归 0，重新固定又排到最后。
  **为什么不能沿用"最近使用倒序"**：那样每用一次某个固定项，它就跳到最前面，用户摆好的排版自己会跑。
  `pinOrder == 0` 只出现在老版本的历史里，`Open` 时按 `lastUsed` 倒序补一遍再落盘。

## 6. 局域网文件互传（对应 dashbeam，已实现）

dashbeam 的骨架是 iroh（QUIC + TLS1.3 + BLAKE3 分块 + ticket + mDNS + 可漫游续传）。
Go 侧一期**没有**照搬这套，落地的是"浏览器下载型"，理由是手机端零安装、实现面最小：

> **方向是单向的：只发不收。** 早先的 `/u` multipart 上传、`%LOCALAPPDATA%\Baozi\inbox\` 落盘目录、
> 首页底部的上传表单已经整条拆除——共享页面对局域网里的人是只读的，任何 POST 都返回 404。
> 保留上传等于把自己的磁盘对整段局域网开一个写入口，用户明确不需要，也不再是"分享"的语义。

1. **服务端**：`internal/share` 起随机端口的 `http.Server`，首页在 `/`，访问码挂在路径前缀 `/s/<code>/`。
   - **链接是一次性的**：`Start(bind, ttl)` 起服务时就定下失效时刻（`<0` 用 `DefaultTTL = 1 小时`），
     `time.AfterFunc(ttl, closeExpired)` 到点直接 `srv.Close()`，之后连端口都连不上；
     请求路径里另有 `Expired()` 检查兜边界（返回 410）。界面「链接有效期」下拉默认 1 小时，
     可选 10 分钟 / 30 分钟 / 2 小时 / 8 小时 / 24 小时，存进 `config.share.ttlMinutes`。
     不做"只要不停止就一直分享"——分享是临时通道，用完即弃。
   - **不再一打开窗口就开服务**：先「添加文件 / 添加文件夹」攒候选（`shareWindow.pending`），
     再点「生成链接」，才是"选文件 → 分享 → 得到一条链接"的顺序。
   - **访问码是 4 位**，字符集排除易混淆字符（`0 O o`、`1 I l i`、`5 S`、`2 Z`、`8 B`），
     用户要能照着窗口把码敲进手机；**校验区分大小写**（同码不同大小写视为不同的码，
     路由层 `subtle.ConstantTimeCompare` 直接比原始字节，不做折叠）。
     生成用 `crypto/rand` + 拒绝采样消除取模偏差。
   - 4 位 ≈ 19 bit，远弱于原来的 16 字节 hex，所以加了**失败熔断**：连续输错 `maxTokenFails = 20`
     次直接 `srv.Close()` 停服，`Breached()` 供界面提示，点「重新生成链接」换新码。
   - **URL 必须带端口**：`http://<ip>:<port>/s/<code>/`。端口是随机的，早先漏拼端口导致链接落到 80 端口打不开
     （`TestStartBindSpecific` 就是被这个 bug 抓出来的）。
2. **绑定网卡**：`Start(bind, ttl)`，`bind` 为空 = 所有网卡（`":0"`），否则听 `bind:0`。
   界面下拉列出 `Bindings()`（接口名 + IPv4，标注虚拟网卡），选择存进 `config.share.bind`，换网卡会重启会话。
   `LANIP()` 的挑选顺序：私有地址且非虚拟网卡 → 任意私有地址 → 第一个候选；
   虚拟网卡按接口名关键词判定（`vmware` / `vmeter` / `virtualbox` / `hyper-v` / `wsl` / `docker` / `mumu` 等）。
   这一步是必须的：装了 VMware / MuMu 的机器上，自动挑到的很可能是虚拟网卡地址，对端根本不存在。
3. **下载**：`/d/<name>`，文件走 `http.ServeContent`（支持 Range）；目录**实时打包成 zip 流**边压边发，不落临时文件。
   只分享一项时根路径直接 302 到 `d/<name>`（打开链接就下载）；多项才渲染列表页。
   **下载链接一律写成绝对路径 `/s/<code>/d/<name>`**：早先写的是相对 `d/<name>`，用户把地址栏结尾斜杠
   丢掉（`/s/ABCD`）再打开页面时，浏览器会把链接解析成 `/s/d/<name>`（把访问码那段当成文件名），点下去必然 404。
   现在 `/s/<code>`（无斜杠）301 到带斜杠版本，两条路都兜住。
4. **单向**：路由只有 `""`/`/`（列表页或单项直下）与 `d/<name>`（下载）两支，其余一律 404。
   服务端不持有任何目录用于写入，`Session` 里没有 inbox 字段，`Start` 也不接收落盘路径。
5. **顺带递一段文本**（`SetText` / `Text`）：`Session` 里挂一段纯文本，列表页把它渲染在文件列表**上方**
   （`html.EscapeString` 转义，内联 JS 复制按钮，`navigator.clipboard` 失败退回 `execCommand` 选区复制）。
   文本**不落盘**——它的用途就是被复制走，让人去下载一个 txt 再打开是绕远路。
   只有文本没有文件时也算一次有效分享：标题写「给你分享的内容」，空清单提示按
   `len(items)==0 && s.Text()==""` 判断，不再误报「还没有选择文件」。
6. **发现**：不广播 mDNS，直接展示绑定的（或自动挑的）IPv4 + 二维码（`skip2/go-qrcode`，纯 Go 无 cgo），手机扫码即可。

与 dashbeam 的差距（明确没做）：无 QUIC、无 NAT 打洞、无公网中继、无分块哈希与断点续传、无端到端加密
（明文 HTTP + 一次性令牌，仅限可信局域网）。Windows 防火墙首次放行需用户点一次确认。

## 6b. 跨网直传（WebRTC，已实现）

局域网那套只在同一网段有用。跨网另起一条：`internal/p2p` + `signal/`（一个 Cloudflare Worker）。

**分工**：局域网走 HTTP（对端零安装、浏览器下载）；跨网走 WebRTC（两端都要装 Baozi、要打洞）。
两条路并存，互传窗口里用页签切换。

**信令**：`signal/src/index.ts` 是一个 Worker + 一个 `Room` Durable Object。
短码就是房间名，`idFromName(code)` 保证同一个码落进同一个实例，两端各挂一条 WebSocket，
DO 只做纯转发（自己只发 `peer_joined` / `peer_left`）。免费计划限制是 **只能用 SQLite 后端的 DO**，
所以 `wrangler.toml` 的 migration 必须是 `new_sqlite_classes = ["Room"]`。

**为什么不用 workers.dev**：`*.workers.dev` 在大陆被 SNI 阻断（TCP 通、TLS 握手失败）。
绑自有域名（`signal.1jia.life`）走 Anycast 就正常，这是实测出来的，不是猜的。

**为什么不做 trickle ICE**：双方把候选收全了再塞进 SDP 一次性发过去，信令上只剩
`offer` / `answer` 两条消息。多等一两秒，换掉"候选先到、描述后到"的乱序处理——
那类 bug 只在特定网络下偶发，非常难查。

**传输协议**：三种帧。
1. 文本 `{"type":"manifest","items":[{"path","size"}]}`（清单可能为空——只发文本时就是空的）；
2. 文本 `{"type":"text","text":"…"}`——**排在清单和数据块前面发**：文本小、先到手，对方能立刻复制，
   不必等几个 G 的文件传完。文本不落盘，接收端只负责摊开给人复制；
3. 二进制 `[4 字节文件序号][32KB 数据]`——靠 SCTP 的有序可靠省掉偏移和逐块确认；
4. 文本 `{"type":"done","total":n}`。

没有校验和：链路层已经管到字节级，再加一层只是自我安慰。
发送端按 `dc.BufferedAmount()` 做反压（超过 1MB 就等），接收端按序号写盘。
**路径必须清洗**（`cleanRel`）：对端构造 `../` 或 `C:` 都跳不出目标目录，重名自动加 `(1)`。

**只发文本也是一次完整传输**：`collect` 的"空清单"不算错、`prepare` 对空清单直接返回 nil，
接收端也不会因为"没文件"就说"什么也没收到"——这条路径在浏览器端实测过（多行中文、`<&>` 原样）。

**没做的**：TURN 中继。`Options.TURN` 留了口子，Cloudflare Realtime TURN 免费额度 1TB/月，
需要在账号里生成短期凭证。对称 NAT 现在会直接报"两端网络都没打通"。

**浏览器当接收端**（`signal/src/receiver.ts`）：信令服务顺带托管一个页面 `/r?code=<短码>`，
里面那段 JS 自己 `new RTCPeerConnection`、自己 `createAnswer`、自己解析同一套帧格式。
于是接收方不用装 Baozi——这对手机最省事，也把"跨网"这件事的入口从"先装个软件"降到"点开一条链接"。

几个必须对齐的点，改任何一处都要两边一起改：

- **帧格式照抄** `internal/p2p/transfer.go`：小端 4 字节文件序号 + 32KB 数据，
  文本帧是 manifest / done。所以 `binary.LittleEndian` 与 `DataView.getUint32(0, true)` 必须成对；
- **不做 trickle**：页面也要等 `iceGatheringState === "complete"` 再把 answer 发出去，
  给 6 秒兜底（Go 端 `gatherBudget` 是 8 秒，留余量）。发早了 Go 会拿到一条没有候选的 answer；
- **角色固定**：发送方永远是装 Baozi 的那台（它有文件、也只有它能读磁盘），
  浏览器永远 `role=receiver`。所以 Go 侧不用改一行——它发 offer，浏览器回 answer；
- **落盘是攒出来的**：页面把每个文件的块塞进 `Blob`，收完再 `a.download` 触发保存。
  Chrome 桌面实测会自动落到下载目录；手机浏览器会拦无手势的下载，所以收完同时亮一个
  「保存文件」按钮作为兜底。这也是为什么大文件建议用客户端收——那边是边收边写。

## 7. 读取文件管理器当前目录（对应 Lertaro `IActivePathCollector`）

只读、不接管对方搜索。按目标进程分路：

1. **资源管理器（Explorer）** —— 已实现。读地址栏控件（见 8.1），
   不照 Lertaro 那样走 `ShellWindows` COM。
2. **Total Commander** —— 已实现。走 TC 的 `WM_COPYDATA` 远程控制协议
   （`TTOTAL_CMD` + `'G'+256*'W'` 查询 / `'C'+256*'D'` 换目录，见 8.1.2），
   不解析窗口标题 —— 用户自定义标题模板会让标题解析失效，只留作兜底。
3. **Directory Opus** —— 已实现（`opus.go`）。读文件显示容器的地址栏
   （`dopus.lister` → `dopus.filedisplaycontainer` → `dopus.ctl.treepath`，见 8.1.3）。
   不用 `dopusrt.exe /acmd`：那要起一个进程、走剪贴板或临时文件，比读窗口慢得多。
4. **XYplorer** —— 已实现（`xyplorer.go`）。没有专用查询命令，走它的脚本通道
   （`WM_COPYDATA` + UTF-16 脚本，脚本自己 `copydata` 回投答案，见 8.1.4）。
5. **xplorer2** —— 已实现（`xplorer2.go`）。没有远程接口，只能读地址栏 `ComboBox`
   的 `WM_GETTEXT`，且必须只看可见的那个（见 8.1.5）。
6. **tessoa** —— 已实现（`tessoa.go`）。它是 Rust + wgpu **自绘**的跨平台文件管理器
   （tessoa.cn，本机实测 v0.30.2），窗口里没有任何标准控件：类名是自建的 `Window Class`，
   UIA 树用 ControlViewWalker 和 RawViewWalker 走下来**都只有顶层一个节点**，
   地址栏那套读法完全用不上。改走它自己提供的命令行 RPC：`tessoa.exe --print-cwd`
   会把查询转给主实例、打印当前活动窗格的目录后立即退出（见 8.1.6）。
7. **OneCommander / Files** —— **不做**。两者都只能走 UI Automation（类名由 WPF /
   WinAppSDK 运行时生成，含 guid 或全机共用，进程名 + UIA 是唯一可靠路径）。
   Go 里手搓 IUIAutomation 的代价与本机收益不成比例，且这两种用户都没装。
8. **Double Commander** —— 未做。Lertaro 没有它的 collector，没有可靠读法可抄。

统一出口是 `internal/win/sources.go` 的 `sourceKind(hwnd)` + `sourceFolder(kind, hwnd)`，
跟随器侧再包一层 `(*DialogFollow).isPathSource` / `folderOf`（见第 8 节），
新增一种来源只要在 `SourceNames` / `sourceKind` / `sourceFolder` 三处加一行。
七种来源都能在设置里单独关掉（`config.pathSources`，见第 11 节）。

## 8. 打开/保存对话框跟随文件管理器（已实现，对应 Lertaro `IFileDialogAdapter` + `IActivePathCollector`）

语义是**全局自动跟随**，不经面板、不需要用户在 Baozi 里选任何东西：

> 打开记事本「另存为」，框停在「我的文档」→ 打开资源管理器把目录切到「下载」→
> 切回那个「另存为」框，**它的路径自己变成「下载」**。

三块拼起来：每隔 200ms 看一眼前台窗口（`internal/win/follow.go`）、
前台是可提供目录的程序（七种来源，见第 7 节）就读出它的当前目录、
前台是文件对话框就按版本号决定要不要跳过去。

### 8.1 记住文件管理器的目录

#### 8.1.1 资源管理器（`ExplorerFolder`）

读的是**地址栏控件**，不是 `ShellWindows` COM：

```
EnumChildWindows( CabinetWClass ) → class == "Breadcrumb Parent" → 它的 ToolbarWindow32
  → WM_GETTEXT（SendMessageTimeout，500ms 超时）
  → 剥掉 "地址: " / "Address: " 前缀
  → resolveFolderText()：绝对路径就校验存在性；本地化名字查已知文件夹表
```

Lertaro 的 `ExplorerPathCollector` 走的是 `ShellWindows`（CLSID `9BA05972-…`）+
`Document.Folder.Self.Path`，还要用 `ShellTabWindowClass` 判多标签里的活动标签。
Go 里手搓 `IDispatch` 又贵又脆，而**资源管理器的地址栏和文件对话框是同一套控件**，
window text 直接就是路径，一条 `EnumChildWindows` 解决。多标签时读 `CabinetWClass`
顶窗（`ShellTabWindowClass` 自己那条读出来是空字符串），所以先用 `GetAncestor(GA_ROOTOWNER)` 上溯。

**已知文件夹要单独还原**（`internal/win/knownfolder.go`）：在「下载 / 文档 / 音乐 /
图片 / 视频 / 桌面」这类文件夹的根上，地址栏给的不是路径而是本地化名字 ——
实测读到 `地址: 音乐`、`地址: 文档`。`isDir("音乐")` 为假，跟随器就只能当没读到，
这正是用户报的"切到下载/文档路径不变"。

名字的来源必须和 shell 用的一致，试过三种只有一种对：

| 取法 | 本机结果 |
|---|---|
| `SHGetFileInfoW(路径, SHGFI_DISPLAYNAME)` | 只给 `Documents`。本机文档被重定向到 `D:\Backup\Documents`，重定向过的目录拿不到本地化名 |
| `SHGetNameFromIDList(pidl, SIGDN_NORMALDISPLAY)` | 对这批目录返回空（`hr != 0`） |
| **`FolderDescriptions\{FOLDERID}\LocalizedName` + `SHLoadIndirectString`** | **`文档` / `下载` / `音乐` …，正是地址栏上那几个字** |

所以做法是：12 个 FOLDERID → `SHGetKnownFolderIDList` 取路径，同时读注册表
`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\FolderDescriptions\{GUID}\LocalizedName`
（值形如 `@C:\Windows\system32\windows.storage.dll,-21770`），用 **`shlwapi.dll` 的
`SHLoadIndirectString`** 解析成本地化名。这是 shell 自己的字符串资源，换任何系统语言
都自动跟着走，不用硬编码"文档"这种词。表缓存一次（`knownOnce`），之后只是查 map；
另外把目录末段名也塞进去，给英文系统 / 没注册本地化名的条目兜底。

注意 `SHLoadIndirectString` 在 **`shlwapi.dll`**，不在 `shell32.dll`（写错会
`Failed to find ... procedure` 直接 panic）。

#### 8.1.2 Total Commander（`TotalCommanderFolder`）

照 Lertaro `Plugins\TotalCommander\Win32\Win32Helper.cs` 的远程控制协议：

| 环节 | 细节 |
|---|---|
| 窗口 | 类名 `TTOTAL_CMD`（主窗口，不是面板子窗） |
| 请求 | `WM_COPYDATA`，`dwData = 'G'+256*'W'`（22343），`wParam = 接收窗口`，`lpData` 是 **ASCII** 的 `"SP\0"`（源/活动面板）或 `"TP\0"` |
| 回复 | `WM_COPYDATA`，`dwData = 'R'+256*'W'`（22354），内容是 **UTF-16** 路径，回给 `wParam` 指定的窗口 |
| 换目录 | `dwData = 'C'+256*'D'`（17475），`lpData = [UTF-8 BOM]path 0x0D 0x00 "S" 0x00`（`"S"` 表示作用于活动面板；**`S` 后的尾 NUL 必须计入 `cbData`**，否则标志位解析不稳）。Baozi 只读不写，这条只给测试探针用 |

命令串必须是 **ANSI** —— 只有"回复用 UTF-16"这一半由 `'W'` 决定，
用 UTF-16 发命令 TC 不理你（Lertaro 源码原话）。

两个实测踩出来的硬约束，缺一个就是"请求发成功、路径永远为空"：

1. **接收窗口必须和发送在同一个 OS 线程。** TC 是同步 `SendMessage` 回来的，
   只有"发送线程自己的窗口"才会被**嵌套投递**，发完立刻就能读到；
   窗口开在别的线程，回复进那个线程的队列，这边读不到。
2. **只能用 `SMTO_ABORTIFHUNG`，不能带 `SMTO_BLOCK`。** 带 BLOCK 时 Windows
   不允许调用线程处理回到自己队列的消息，回复照样被丢掉。
   `tools/tcprobe` 把四种组合跑了一遍，结论是：

   ```
   SendMessage                      -> 回调次数=1 路径="D:\Backup\Documents\"
   SendMessageTimeout(no BLOCK)     -> 回调次数=1 路径="D:\Backup\Documents\"
   SendMessageTimeout(BLOCK)        -> 回调次数=0 路径=""          ← 之前用的就是这个
   SendMessage(wParam=0)            -> 回调次数=1 路径="D:\Backup\Documents\"
   ```

这两条不是 TC 独有的，凡"发一条消息、对方同步回投答案"的协议都适用，
所以收发那一套抽到了 `internal/win/copydata.go`（`queryCopyData`），
**Total Commander 和 XYplorer 共用**：`LockOSThread` → 注册一次窗口类
`BaoziQueryWnd` → 建 message-only 窗口 → `build(receiver)` 生成 payload →
`sendMsgNoBlock` → 读回复 → `DestroyWindow`。`build` 是回调而不是现成的字节：
XYplorer 的脚本必须把接收窗口的 hwnd 拼进去，只能建完窗口才生成。
`sendMsg` 的语义因此仍是两个：`sendMsg`（带 BLOCK，给对话框写文本用）和
`sendMsgNoBlock`（查询专用）。两者都是 `SendMessageTimeoutW` + 500ms 超时，
对方卡住也不会拖住跟随器。

高权限场景另外调了 `ChangeWindowMessageFilterEx(receiver, WM_COPYDATA, MSGFLT_ALLOW, 0)`：
Baozi 若以管理员身份运行，UIPI 会静默丢掉普通权限进程的回复。

#### 8.1.3 Directory Opus（`OpusFolder`）

照 Lertaro `Plugins\DirectoryOpus\DirectoryOpusPathCollector.cs`。窗口层次是：

```
dopus.lister（lister 主窗口，一个 Opus 窗口）
  └ dopus.filedisplaycontainer（文件显示容器，一个标签页一个）
       └ dopus.ctl.treepath（活动标签的地址栏，window text 就是完整路径）
```

非活动标签没有地址栏，读容器自己的 window text（Opus 把每个标签的目录放在自己的标题上）。

**双栏怎么选"用户正在看的那一栏"**：先按 lister 标题唯一定位某一栏（标题前缀就是
目录名），标题判断不出来再看键盘焦点落在哪个容器里（`GetGUIThreadInfo` 取
`hwndFocus`，再沿父链上溯）。两条都判不出就**返回空、不导航** ——
Lertaro 的第三层"按几何距离猜横竖分屏"故意没做：跳到另一栏的目录，
比这一下不跳更让人难受。

另外过一遍 `resolveFolderText`：Opus 报出来的可能是本地化拼写（`C:\用户\…`），
那不是存在的路径。Lertaro 的 `LocalizedPathResolver`（逐段翻译目录名）没做，
没有装 Opus 的机器可验，宁可不写这段猜逻辑。

#### 8.1.4 XYplorer（`XyplorerFolder`）

照 Lertaro `Plugins\Xyplorer\Win32\Win32Helper.cs`。窗口类名 `ThunderRT6FormDC`
（VB6 主窗体，任何 VB6 程序都用这个），所以必须再过一道**进程名 `xyplorer*`** 才算数。

XYplorer 没有 TC 那样的查询命令，它暴露的是"跑一段 XYplorer 脚本"：

| 环节 | 细节 |
|---|---|
| 请求 | `WM_COPYDATA`，`dwData = 0x00400001`（RUN_SCRIPT），`lpData` 是 **UTF-16** 的脚本文本 |
| 脚本 | `::$return = "<curpath>"; copydata <接收窗口>, "$return", 2;` |
| 回复 | 脚本自己用 `copydata` 回投，`dwData = 2`，内容是 UTF-16 路径 |

`$return` 在引号里是给 XYplorer 做字符串替换的，不是字面量；接收窗口的 hwnd 必须
以十进制数字直接写进脚本 —— 这就是 `queryCopyData` 的 `build` 要拿到 receiver 的原因。
脚本按 UTF-16LE 编码、不带尾 NUL（和 .NET `Encoding.Unicode.GetBytes` 一致）。

#### 8.1.5 xplorer2（`Xplorer2Folder`）

照 Lertaro `Plugins\Xplorer2\Win32\{Win32Helper,WindowProbe}.cs`。框架类
`ATL:ExplorerFrame`，同样要配进程名 `xplorer2*`。xplorer2 没有远程控制接口，
只能读窗口：地址栏是一个 `ComboBox`，**`WM_GETTEXT` 的回复才是路径**，
同一个句柄 `GetWindowText` 返回空（Lertaro 专门量过这一点）。

两个坑：

1. 一帧里有**两个** `ComboBox` —— 传统地址栏（显示面包屑时它被隐藏）和面包屑自己那个。
   隐藏的那个还留着上一次访问的路径（实测活动面板在 `D:\Notes` 时，隐藏的传统栏
   仍报 `C:\Windows`）。所以**只看可见的**：看不见的一律不算，宁可返回空。
2. 编辑中途 `ComboBox` 会短暂报出纯数字垃圾，还有 `::` 开头的 shell 命名空间
   （此电脑之类），都不是文件系统路径，要滤掉（还要 `filepath.IsAbs` + `isDir`）。

面包屑那个自己的窗口在 ATL 里是**无名类**（`ATL:%p`），只能按**文字**认它 ——
类名对不上，window text 是 `CrumbBar`。认出来之后，焦点面板那条 `ComboBox`
的父窗口就是它，优先用；其余可见的 `ComboBox` 当兜底。

> **这三种（Opus / XYplorer / xplorer2）没有在真机上验证过** —— 用户明确说这几样
> 他一个都没装。实现完全照 Lertaro 的 collector 写，能验证的只有：
> 未安装时 `IsDirectoryOpus` / `IsXyplorer` / `IsXplorer2` 一律为假（`tools/folderprobe`
> 在本机跑出来只有资源管理器与 TC），因此不会把普通窗口误判成目录来源、不会乱跳；
> 读不到时返回空串，跟随器按"没读到"处理，不导航也不崩。
> 装了这几样的人如果读出来不对，`tools/folderprobe` 会打印容器 / `ComboBox` /
> 地址栏原文，照着改就行。

#### 8.1.6 tessoa（`TessoaFolder`）

**已在本机真机验证**（v0.30.2，管理员权限运行）。tessoa 是 GPU 自绘的，
所以第一步先确认常规手段用不了：

- 窗口类名是自建的 `Window Class`（不是 `CabinetWClass` 那类系统类），
  按其类名认人不可靠，只能按进程名认；
- 窗口里没有 `Breadcrumb Parent` / `ToolbarWindow32`；
- 它**确实**加载了 `uiautomationcore.dll`，但 `AutomationElement.FromHandle`
  之后用 `ControlViewWalker`、`RawViewWalker` 两种视图走，都只有顶层 Window 一个节点
  （自绘 UI 没建无障碍树）。所以 UIA 这条路也是死的。

它自己给了命令行 RPC。二进制里能看到这条命令族的名字：
`open / open-window / reveal-window / dev-extension / open-url / query-cwd`，
命令行参数是 `--print-cwd`（还有 `--print-cwd=` 的带参形式）。实测：

```
D:\1tools\tessoa.exe --print-cwd        →  打印当前活动窗格的目录后 exit 0
D:\1tools\tessoa.exe --open D:\work\baozi → 日志里 "已转发给主实例（pid=7016）→ 本进程退出"
```

也就是说 `--print-cwd` 问的是**已经在跑的那个实例**，不是"新进程自己算出来的目录"：
实测 `--open` 换目录之后立刻再 `--print-cwd`，返回值跟着变了（`D:\` → `D:\work\baozi`）。

代价与缓存：

| 场景 | 单次 | 说明 |
|---|---|---|
| 热态 | **16 ms** | 0.1MB 进程启动 + 管道往返，实测 6 次 16/16/16/16/16/16 |
| 冷态 | **43 ms** | 文件不在系统缓存里时 |

跟随器三处会调它：前台切到 tessoa 那一下、**tessoa 的窗口标题变了**（见下）、
以及文件对话框弹出时对"上一次活动的那个来源"现场重读一次（这一次不吃缓存）。
`tessoaCacheTTL = 400ms` 让挨得很近的轮询调用共用一个结果，省掉一次进程启动。

**4 窗格 / 切布局：前台窗口根本不变。** tessoa 是单窗口自绘，切窗格、切布局、
在同一个窗格里进目录，`GetForegroundWindow` 一直是同一个 hwnd。跟随器原来只按
"前台窗口变化"决定要不要读目录，于是 4 窗格布局下把焦点挪到左下角、或者切换布局，
它完全看不见 —— 日志里只有"记住目录"停在上一次，打开框自然不跟着走。
现在拿**窗口标题当变更信号**：tessoa 的标题是 `<活动目录名> — <布局名> — tessoa (管理员)`
（实测 `1tools — normal — tessoa (管理员)`），标题一变就先 `--print-cwd` 问完整路径
（节流 500ms，读标题本身要发 WM_GETTEXT，常态零开销）。标题只有目录名、两个不同目录
同名时会漏一次，靠对话框弹出时那次现场重读兜住。

三个必须处理的坑：

1. **自更新会让进程镜像改名**。tessoa 更新时先把正在运行的 exe 改名成 `tessoa.old.exe`，
   再写入新的 `tessoa.exe` —— 进程还活着，但它现在的镜像名是 **`tessoa.old`**。
   实测本机 pid 7016 就是 `D:\1tools\tessoa.old.exe`。所以：
   - `IsTessoa` 的判据是 `tessoa` 本身或 `tessoa.*`（排除插件进程 `tessoa-av` / `tessoa-office`）；
   - 发查询时**优先用窗口所在目录里的新 `tessoa.exe`**（它启动时认得同一个主实例锁，
     会转发过去而不是再开一个窗口），新 exe 不在才退回进程自己的镜像。
2. **没有实例可转发时会自己开窗口**。也就是说如果 tessoa 已经被关掉而我们还在问，
   等于替用户把软件打开了。所以查询前先 `IsWindow(root)` 确认窗口还在；
   拿不到窗口句柄直接返回空。
3. **欢迎页没有真实目录**。tessoa 停在 `@Welcome` 标签时 `--print-cwd` 给的是盘根
   （本机是 `D:\`）。它仍然是个合法目录，跟随器照用；这种情况下不要指望它跟着切。

多开实例的边界：`--print-cwd` 总会找"主实例"，所以用户同时开了两个 tessoa 时，
读到的是主实例的目录，可能与当前前台那个窗口不一致。这是它 CLI 的语义决定的，
我们不做额外猜测（猜错会把对话框拽到用户没在看的目录）。

读回来的输出还要过一遍 `tessoaParse`：挡空行、多行、带引号、带 `\\?\` 前缀；
**并且必须是真实存在的目录**才认。盘根 `D:\` 先按原样试，试不过才去掉尾部分隔符 ——
否则 `D:\` 会被削成 `D:`，那是"当前盘当前目录"，不是盘根。

#### 8.1.7 Arke（`ArkeFolder`）

Arke（Koimsurai/Arke，本机 v0.8.0-rc.1，Rust + GPUI）对外有一个版本化的 JSON 接口
（仓库 `docs/integration.md`），是它承诺给启动器 / 对话框助手用的那一条：

```
arke.exe --api -        # stdin: {"v":1,"cmd":"state"}；stdout: 一行 JSON
```

回复里有**每个窗口 → 标签页 → 窗格**，每层恰好一个 `active: true`；
窗格的 `location.kind` 为 `folder` 时带 `path`。双栏时取活动标签里的活动窗格，
不去猜另一栏 —— 接口文档就是这么建议的（`home` / `search` / `recycle_bin` /
`remote` / `other` 都没有"能送进对话框的目录"，一律当没有）。

为什么不读 `%APPDATA%\Arke\settings.json`：那是上次退出时的会话快照，
运行中切标签、切窗格、进目录都不写回；接口回的才是实时状态。

几个和 tessoa 不同的点：

- **窗口类名是 `ArkeWindow`**（弹出菜单是 `ArkeWindow.Popup`），`IsArke` 直接按类名认，
  `GetClassName` 可靠；但不能用 `FindWindowW` 按类名跨进程找 —— Arke 的窗口类没有
  注册成全局类，实测 `FindWindowW("ArkeWindow")` 返回 0（本机 go test 里改成
  `EnumWindows` + `GetClassName` 才找得到）。
- **`state` 不会启动 Arke**：没有实例时直接回 `{"v":1,"windows":[]}`；所以不必像
  tessoa 那样担心"问一次就把软件打开"。仍然先 `IsWindow(root)`，避免窗口刚关时
  问到别的窗口的目录。
- 有多个 Arke 窗口时，先按 `hwnd` 找调用方那个窗口，找不到才退回第一个 active 窗口。
- 单次查询要起一个短命 `arke.exe --api` 客户端（已有实例时走它的单实例管道转发），
  因此和 tessoa 一样做 400ms 缓存；对话框弹出时 `ArkeFolderFresh` 不吃缓存，
  保证双栏里刚切过去的那一栏能立刻生效。
- 查询结果还要过 `arkeFolderFromJSON`：要求 `v==1`、路径去掉 `\\?\` 前缀、
  并且**必须是真实存在的目录**才认。单测覆盖了活动窗格、活动标签、按 hwnd 选窗口、
  `home` 不猜、目录已被删除、空 / 版本不符、长路径前缀。

### 8.2 什么时候跳（照抄 `FileDialogNavigationTracker.cs`）
状态只有两样：`lastDir`（最近一次文件管理器待过的目录）和 `version`（目录变一次 +1），
外加每对话框一个 `seen[hwwnd] = 它上次同步时的 version`：

| 情形 | 动作 |
|---|---|
| 前台是文件管理器（七种来源之一） | 记下它的窗口（`lastSource`），**版本号 +1**（表示"刚去看过"），顺手读一次目录更新 `lastDir` |
| 对话框成为前台，且**第一次见** | 只有上一次活动的正是那个文件管理器窗口，才**现场重读**并跳 |
| 对话框成为前台，且**以前见过** | 版本号变过就**现场重读**并跳；版本没变就不动（中间夹了谁不影响） |
| 前台是文件管理器，目录与 `lastDir` 不同 | `lastDir = dir`，版本号再 +1 |

注意"版本号 +1"不只在目录变化时发生：只要文件管理器窗口重新成为前台就 +1，哪怕
它停在原地。否则用户"切到 Arke 看一眼、切回框"会什么都不发生，必须先在 Arke 里
换一次目录才生效 —— 实际用起来就像"只切过去不算数"。多 +1 一次只影响差值，
不影响"变过/没变"的判定。

「第一次就直接跳」是唯一偏离 Lertaro 原文的地方，也是必须的：原来照抄"第一次只登记、
等版本再变一次才跳"，而版本在框出现那一刻已经被现场重读升到最新了 —— 于是**每个新
框都只会被登记、永远不跳**，用户看到的就是"打开框根本不跟随"（本机日志里 34 条
"送到"全是"框开着的时候又在文件管理器里换了目录"这种碰巧成立的路径）。
第一次跳用的是**刚刚现读**的 `lastDir`，所以库里那个"程序自己指定了目录，不该覆盖"
的担心并不成立：我们要跳的正是用户此刻在文件管理器里看的位置。

「版本没变就不动」是 Lertaro 源码里明写的坑：用户手动在框里换过目录，
下次切回来不能被打回旧目录。

**「切回时现场重读」**对应 Lertaro 里那行 `previousWasPathProvider && readProviderPath()`：
如果只在"轮询看到文件管理器"那一刻读目录，用户换目录的瞬间可能正好落不到那一次采样上，
表现就是"第一次能跟，换个目录再切回去就不跟了"。改成切回对话框的时刻现读，读到的
永远是最新的；只记住 hwnd、不缓存内容。

**Alt+Tab 会夹一个应用主窗口（2026-11 修）**：框开着 → Alt+Tab 去 Arke（换没换目录
都行）→ Alt+Tab 回来时，Windows 可能先把记事本主窗切成前台（`Notepad`，不是那个
`#32770`），用户再点一下框。这时紧邻的上一个前台是记事本而不是 Arke，只认
`prev == lastSource` 就会漏掉——本机实测"鼠标直接点框能跟，Alt+Tab 回来不跟"。
现在的判断抽成 `shouldFollow(seenVersion, seenBefore, version, prevWasSource)`：

- **第一次见**：仍然只认"上一次活动的就是文件管理器"，避免从任何程序打开的框被
  一个旧目录拽走；
- **以前见过**：只要 `seen[hwnd] != version` 就跳，和中间夹了哪个窗口无关。版本号
  变过说明用户切到过文件管理器（哪怕它停在原地）；版本没变说明只是切了别的窗口，
  用户手动选的目录不会被覆盖。
- 第一次没跳也要把当前版本记成基线，否则之后用户切到文件管理器再回来，
  这个框会永远被当成"第一次"，仍然不跳。

`lastExplorer` 已改名 `lastSource`：现在"目录来源"有七种，判断"上一次活动的是不是
同一个来源窗口"和"能不能现读"都走 `(*DialogFollow).isPathSource()` /
`folderOf()` 两个分派点（它们是方法不是包级函数了，因为要先按设置过滤来源）。

### 8.3 怎么跳（`NavigateDialog`，照 `StandardFileDialogAdapter`）

不是注入 DLL，而是**往"文件名"框写完整路径，再触发对话框的默认按钮**
（新式 Common Item Dialog 的文件名框仍是一个普通 `Edit`，进程外能写）。

```
IsFileDialog(hwnd)?                       ← #32770 且内部有 "Breadcrumb Parent"
EnumChildWindows（递归） → 第一个 class == "Edit"    ← 文件名框（ComboBoxEx32/1148 里面）
  → 目录补尾 "\"，WM_SETTEXT 写入
  → 父窗口 WM_COMMAND((EN_CHANGE<<16)|ctrlID)
  → 回读校验；不一致就绝不触发（否则会把框里原来那个文件名提交掉）
  → 250ms 后 PostMessage(hwnd, WM_COMMAND, IDOK)
```

**为什么是 `WM_COMMAND(IDOK)` 而不是发回车键**：对话框的"回车=按默认按钮"是消息循环
里的 `IsDialogMessage` 完成的；`PostMessage(WM_KEYDOWN, VK_RETURN)` 直接投给文件名
`Edit`，绕过了这一步，多数时候只把路径留在编辑框里、地址栏不动（用户截图里的现象：
地址栏还是"文档"，文件名框里是 `D:\Backup\Downloads\`）。默认按钮实际收到的就是
`WM_COMMAND(IDOK)`；路径以反斜杠结尾时外壳把它当目录导航，不会保存文件
（实测对话框保持打开，地址栏变成目标目录）。

三个必须注意的点：

- **必须 `EnumChildWindows` 递归**。`FindWindowExW` 只扫直接子窗口，而 `Breadcrumb Parent`
  和文件名框都在两三层以下 —— 用它就会"明明有对话框却认不出来"。
- **绝不 `AttachThreadInput`**。它把本线程和对话框线程的输入队列绑死；
  而 Go 的 goroutine 会在 OS 线程之间迁移、那个线程之后还会被 runtime 复用，
  等于把对方的输入队列挂在一个不再处理消息的线程上 —— 对方窗口就此永久无响应。
  实测代价：在记事本的"另存为"上直接把记事本按死（用户报的"切回 notepad 卡死"）。
  现在一个 `AttachThreadInput` 都没有，也不自己抢前台（切回对话框这个动作本身
  就保证了它是前台）。
- **跨进程消息一律带超时**：`SendMessageTimeoutW` + `SMTO_BLOCK|SMTO_ABORTIFHUNG` + 500ms。
  裸 `SendMessage` 只要对方卡一下就把调用方（进而把整个跟随器）拖死。

面板那条手动路（唤起时记住对话框、`Enter` 改成"送过去"）已经删掉：它不符合这个语义，
而且会顶掉 `Enter` 原本的"打开文件"。开关是 `config.json` 的 `"dialogFollow": false`。

实测（2026-11-07，WinForms `OpenFileDialog` 靶子 `dist\dlg-target.ps1`，
`tools/followtest` 在一个进程里连跑两轮 —— 第二轮专门覆盖"再换个目录再切回来"）：

```
① 另存为框 0x4c0426 初始地址: "地址: 文档"
② 资源管理器切到 D:\work\baozi
③ +500ms 地址: "地址: D:\work\baozi" 对话框还在=true 卡死=false
④ 资源管理器切到 C:\Windows\System32
⑤ +500ms 地址: "地址: C:\Windows\System32" 对话框还在=true 卡死=false
```

（六次采样里 `卡死` 全是 false，`IsHungAppWindow` 判的。）

**已知文件夹那条**（这一轮补的，正是用户报的"切到下载/文档路径不变"）：

```
① 另存为框 0xa135a 初始地址: "地址: 文档"
② 资源管理器切到 D:\work\baozi
③ +500ms 地址: "地址: D:\work\baozi"      对话框还在=true 卡死=false
④ 资源管理器切到 D:\Backup\Downloads → 地址栏显示 "地址: 下载"
⑤ +500ms 地址: "地址: 下载"                对话框还在=true 卡死=false
```

**Total Commander 那条**（`FOLLOW_TC=1`，TC 11.50 x64；用 CD 命令替它换目录）：

```
② 让 TC 前台并切到 D:\work\baozi        → TC 当前目录="D:\work\baozi"
③ +500ms 地址: "地址: D:\work\baozi"    对话框还在=true 卡死=false
④ 让 TC 前台并切到 D:\work\baozi\internal → TC 当前目录="D:\work\baozi\internal"
⑤ +500ms 地址: "地址: D:\work\baozi\internal" 对话框还在=true 卡死=false
```

Baozi 同一时刻的日志：

```
跟随: 第一次见到对话框 0x4c0426，只登记（版本 0）
跟随: 记住目录 "D:\work\baozi"（版本 1）
跟随: 对话框 0x4c0426 版本 0→1，送到 "D:\work\baozi"
对话框跟随: 0x4c0426 -> D:\work\baozi
跟随: 记住目录 "C:\Windows\System32"（版本 2）
跟随: 对话框 0x4c0426 版本 1→2，送到 "C:\Windows\System32"
对话框跟随: 0x4c0426 -> C:\Windows\System32
```

**接完五种来源之后的回归**（2026-11-07 23:35 / 23:37，同一天连跑两轮；
这一轮把收发抽成 `copydata.go`、分派改成按来源开关过滤的方法，所以必须重测一遍）：

```
# FOLLOW_TC=1（TC 当来源）
① 另存为框 0x25e0e5e 初始地址: "地址: 文档"
② TC 切到 D:\work\baozi            → TC 当前目录="D:\work\baozi"
③ +500ms 地址: "地址: D:\work\baozi"        对话框还在=true 卡死=false
④ TC 切到 D:\work\baozi\internal    → TC 当前目录="D:\work\baozi\internal"
⑤ +500ms 地址: "地址: D:\work\baozi\internal" 对话框还在=true 卡死=false

# 资源管理器 + 已知文件夹（FOLLOW_TITLE=下载）
① 另存为框 0xc7114c 初始地址: "地址: 文档"
④ 资源管理器切到 D:\Backup\Downloads → 面包屑显示 "地址: 下载"
⑤ +500ms 地址: "地址: 下载"          对话框还在=true 卡死=false
```

两轮各六次采样，`卡死` 全是 false；跟随器启动日志同时也报了来源清单：

```
对话框跟随: 已启动（来源：资源管理器 / Total Commander / Directory Opus / XYplorer / xplorer2）
```

**五种来源的识别在同一天用 `tools/folderprobe` 核过**（本机没装 Opus / XYplorer /
xplorer2，所以只应看到资源管理器与 TC，一个都不能多）：

```
[TC] 发 0x1d07fc：ok=true dwData=0x5752（='R'+256*'W'） 回复路径="D:\Backup\Documents\"
[TotalCommander] 0x1d07fc  目录="D:\Backup\Documents"
[资源管理器] 0xa1214 title="System32" 地址栏原文="地址: C:\Windows\System32" 解析目录="C:\Windows\System32"
[资源管理器] 0x71278 title="梦境寻忆" 地址栏原文="地址: D:\Backup\Downloads\梦境寻忆" 解析目录同上
[资源管理器] 0x8830e02 title="本地磁盘 (D:)" 地址栏原文="地址: D: \" 解析目录="D:\"
```

（这同时是"不会误判"的证据：机器上跑着一堆普通程序，Opus / XYplorer / xplorer2
三条判定一个都没触发。）

留下的探针：`tools/followtest`（整条链路，`FOLLOW_NOTEPAD=1` 改用真记事本、
`FOLLOW_SECOND=<目录>` 指定第二轮目标、`FOLLOW_TC=1` 改用 Total Commander、
`FOLLOW_TITLE=<标题>` 指定已知文件夹窗口的本地化标题）、`tools/exploredump`
（资源管理器目录）、**`tools/folderprobe`（五种来源的识别 + 目录读取 + 已知文件夹映射表；
装了 Opus / XYplorer / xplorer2 时还会打印容器、`ComboBox` 与地址栏原文）**、
**`tools/tcprobe`（TC 协议单测，四种投递方式对比；`-cd <路径>` 只换目录）**、
`tools/dlgprobe2`（枚举 + 单次导航 + 读文件名框）、`tools/dlgdump`（打印对话框全部子控件）。

> 记事本的自动化发键在这台机器上抢不到前台（`Ctrl+Shift+S` 四次都没弹出另存为框），
> 所以"记事本现场"只能由用户手动复现；功能路径与 WinForms 靶子完全相同
> （都是 `#32770` + Common Item Dialog）。

### 8.4 那些弯路：为什么只能走"文件名框 + 默认按钮"

DLL 注入那条（`WH_CBT` 捕获 + `IShellView::Navigate`）需要往目标进程里塞代码，
本机没有 C 编译器（`gcc/g++/cl/tcc/zig/clang` 全无），Go 的 `-buildmode=c-shared` 出 DLL
再改 COM vtable 也不现实，还会被杀软盯上。放弃。

早期对着一台真实 Win11「打开」对话框（记事本）试过的所有手段：

| 手段 | 结果 |
|---|---|
| `SetWindowTextW` 写地址栏 `Edit(id=41477)` | **文本进去了**（读回来确认），但地址栏与列表不动 |
| `WM_COMMAND(CBN_SELENDOK / CBN_KILLFOCUS)` 发给 band / ComboBoxEx32 / 对话框 | 全部被无视 |
| `SendMessage`/`PostMessage` 造 `VK_RETURN`（补扫描码、补 `WM_CHAR`） | 无反应 |
| 真实 `SendInput`（Unicode 键事件，26 个全部被接受）写**地址栏** | 编辑框内容不变 |
| `SetFocus`（含 `AttachThreadInput`） | 无效；`GetGUIThreadInfo` 读对话框线程 `HwndFocus = 0x0` |
| `Ctrl+L` | 这版对话框不切编辑态（地址栏 `Edit` 仍 `visible=false`） |
| 真实鼠标点面包屑右侧空白 | **有效**：地址栏 `Edit` 变成 `visible=true`，但打字仍进不去 |
| 真实鼠标点标题栏抢前台 + `SetForegroundWindow` | 后台进程会被前台锁拒绝（前台仍是别的程序） |

差别只有一处：**这些试的都是"地址栏"（`Edit(id=41477)`），而不是"文件名"框**。
地址栏写进去只改显示、不回灌给 Shell 视图；文件名框是对话框真正会读的那个输入，
写它、再触发默认按钮，Shell 自己就会导航。

### 现在也给的后备

`Ctrl+Shift+C` 复制选中项所在目录到剪贴板：在框里 `Ctrl+L` → `Ctrl+V` → 回车也能跳。
只在"那套 WM_SETTEXT 被外壳拒掉 / 对话框被别的程序挡住"时还要手工救一次。
`dirOf()` 负责"目录给自身、文件给所在目录、`shell:` 虚拟项无目录"的口径。

## 9. 索引服务化（已实现）

最初索引跑在主进程里（`filepath.WalkDir`），179 万条要分钟级，且必须是普通权限。
现在多了一个可选的服务进程，形态如下：

```
BaoziIndex（服务，LocalSystem）                    baozi.exe（用户，普通权限）
  FSCTL_QUERY_USN_JOURNAL   → 位点                读 index.bin（<300ms）
  FSCTL_ENUM_USN_DATA       → 全量 $MFT 记录      再叠 index.delta
  FSCTL_READ_USN_JOURNAL    → 增量（**直接改表**）  搜索只读这份快照
        ↓ 写                                      ↑
  index.bin + index.delta + index.progress.json + index.heartbeat
        └──────────── 30s 轮询 + 面板唤出时立刻追 ────────┘
```

增量不再是"什么时候重扫"的触发器，而是直接改服务内存里那张表；
`index.bin` 只在整理（墓碑/垃圾攒够）和兜底（6 小时）时重写，
中间的变化走 `index.delta` 这个几十 KB 的小文件。详见「增量：实时改表」。

### 服务挂了要能自己回来

默认安装出来的服务，SCM 的失败恢复动作是空的（`sc qfailure` 实测 `RESET_PERIOD 0`、
无动作）。本机就撞上过：`BaoziIndex` 在 11/19 00:18 意外停止（事件 7034），
因为没人拉它，索引一直停在那一刻，搜索看起来"不实时"，直到手动 `Start-Service`。

因此 `--install` 里做了两件事（用户不需要懂 SCM）：

- 用 `ChangeServiceConfig2(SERVICE_CONFIG_FAILURE_ACTIONS)` 写恢复动作：
  重启 5s / 15s / 60s / 5min / 15min，`RESET_PERIOD=86400`；并置
  `FAILURE_ACTIONS_FLAG=TRUE`，让非崩溃的异常退出也触发恢复。
- 服务循环自己退出的路径（打开卷失败、读配置失败……）不再挂着 `RUNNING`
  什么都不干，而是报 `StopPending` 并返回退出码 1，交给 SCM 按上面的动作重启。
  以前这种"进程活着、索引死了"的状态，主程序只能从心跳变旧看出来。

真正崩溃（进程被杀、未处理异常）由 SCM 直接计数，同样按 5s 回来。

### 三条原语

| FSCTL | 作用 | 实测 |
|---|---|---|
| `0x000900f4` QUERY_USN_JOURNAL | 拿 journalID / FirstUsn / NextUsn | 立即 |
| `0x000900b3` ENUM_USN_DATA | 按 FRN 顺序吐整个卷的 $MFT 记录 | C 盘 103 万条 **3.05 秒** |
| `0x000900bb` READ_USN_JOURNAL | 从 NextUsn 起读增量 | 每条变化都拿到（**不等内核**，见下） |

`USN_RECORD_V2` 的布局（偏移即代码里用的）：`RecordLength@0` `Major@4`
`FRN@8` `ParentFRN@16` `Usn@24` `Reason@40` `FileAttributes@52` `NameLen@56`
`NameOff@58`，名字是 UTF-16。只认 MajorVersion==2（卷上极少见 3/4）。

### 三件必须知道的坑

1. **FRN 要掩码**。记录里的 FileReferenceNumber 是 64 位，高 16 位是序列号，
   低 48 位才是 $MFT 记录号。路径还原必须 `frn & 0x0000FFFFFFFFFFFF` 再比——
   否则父节点写的是「序列号|5」，而根目录常量是 5，整棵树一条都接不上
   （实测就是这里挂了，索引只剩两个盘符）。
2. **USN 记录没有大小和时间**。这两项在 `$STANDARD_INFORMATION` 属性里，要另读；
   所以由 USN 建出来的索引 `SizeKB/ModSec` 一律为 0。搜索排序只看名字/类型/频次，
   界面也不显示这两项，所以不补。
3. **隐藏文件的判定不能靠路径**。USN 只给属性位，直接用 `FILE_ATTRIBUTE_HIDDEN`；
   隐藏**目录**照常进（和 WalkDir 一致，见 §3 的扫描段）。

### 路径还原

没有目录可走，只能靠父子关系拼：

1. 枚举时丢掉 `$` 开头的 NTFS 元数据（`$Mft`/`$RmMetadata`/`$Txf`…，父链指向卷根外）；
2. 按 `parent` 排序，DFS 时用 `sort.Search` 二分定位子集——**不给每个目录建 map**
   （147 万目录各带一个 slice header 就要 35 MB）；
3. 从 `RootFRN=5` 往下走，边走边用 `[]byte` 累积路径（一个 buffer 反复截断，
   不为每条都分配一个字符串）；
4. 父节点被排除/删掉的子树整棵不可达，行为与 WalkDir 跳过 `SkipDir` 一致；
   reparse point（符号链接、目录联接）不跟随，避免环路。

### 增量：实时改表（append-only + 墓碑 + FRN 映射）

索引是 arena + 定长记录（`Parent` 存数组下标）的快照。早先因此不敢原地增删，
增量只当"什么时候值得重扫"的触发器，代价是每 5 分钟来一次 16 秒的全量重建
（1515 次 / 累计 ~7 小时 CPU，就是任务管理器里那个常年 8%）。

现在改成 Everything 那套：**只追加、不压缩**。

- 新增条目 append 到 `Nodes`/`Names` 末尾，已有下标一个都不动；
- 删除只记一条墓碑（v4 起是 `dead` 集合里的一个下标，不再原地改 `Kind`——基座是只读映射页），槽位留着。这一步不能省：NTFS 删一个
  目录时 USN 里只有目录本身一条记录，里面几千个文件没有记录，父子链必须留着，
  才能让子条目跟着失效（`Table.Dead(i)` 沿 `Parent` 上溯，深度上限 256）；
- 按 FRN 找条目靠 `index.FRNMap`（开放寻址哈希，键/值拆成两张并行数组，
  12 字节一槽、装载率 0.8，179 万条约 34MB）。**服务每次启动都会全量一次**，
  顺便把这张表建起来，所以它不落盘、`index.bin` 格式完全不变。

**跨卷的坑**：NTFS 的 FRN 只在卷内唯一，两盘的卷根 FRN 都是 5。直接按 FRN 查
会把 C 盘根下的新文件认成"父在 D 盘根"。所以登记映射时把盘符编进高 16 位
（`usn.volKey`：`A`=0…`Z`=25，低 48 位是 FRN 本体，正好和 `frnMask` 对齐）。

**什么时候还是得全量**：墓碑 ≥ 20000、名字 arena 追加 ≥ 8MB、增量条数 ≥ 5000，
或者离上次全量超过 6 小时（兜底，防止增量路径漏记/错记）。正常使用下一天
一两次，摊开就是 0.0x%。

**主进程怎么知道有新条目**：服务把每条变化攒成一条 `DeltaOp` 写进 `index.delta`
（`{base, seq, ops}`，每 5 秒一批，通常几十条）。`base` 是这次全量的代次
（`index.progress.json` 的 `base`，纳秒时间戳），`seq` 是这一代的第几批。

`DeltaOp` 带的是**下标**而不是 FRN：服务端和主进程从同一份 `index.bin` 出发、
按同一顺序 append，下标天然对齐，主进程因此不用再维护一张 25MB 的 FRN 表。
改名用 `Kill` 字段承载"先失效旧条目"，枚举感觉就是一条 op。

主进程合并走 **copy-on-write**：复制当前表（35MB Nodes + 40MB Names，约 15ms）
→ `ApplyOps` → 原子替换 `a.cat`。为什么不原地改：搜索是无锁遍历 179 万条的
（几十毫秒），身后有人 append 会让它读到越界数据。`ApplyOps` 会校验 `Idx` 正好
等于下一个空位，对不上（说明载入的是旧一代）就整体放弃、等 bin 换代。

触发节奏：后台 30 秒看一次（`indexWatchInterval`），**面板唤出时再立刻追一次**
（`application.show` → `refreshIndex(true)`）。所以"用户看得见的那一刻，索引一定
是最新的"，平时每两秒就有人动文件的机器也不会被反复复制 75MB。

**数据目录必须排除**：服务每 5 秒写 `index.delta`（还有 `.tmp` 改名、heartbeat、
progress），每个动作都有 USN 记录。不排除的话它把自己的写入当成"用户动了文件"
再索引一遍、再攒增量、再写盘——自己喂自己。默认规则里已经有
`AppData\Local\Baozi`，`ruleFor()` 再按 `--data-dir` 的绝对路径补一道。

**为什么阈值这么定**（这是"常驻 8% CPU"的根因，留作历史）：一次重建 15~16 秒、
几乎占满一个核，`baozi-index.log` 里累计 1515 次、平均 16.59 秒、总计约 7.0 小时
CPU。删 4000 个文件时变化连续涌来，于是出现「重建 15 秒 → 攒 16 秒 → 再重建」的
背靠背连发（21:23:04 / 21:23:21 / 21:23:37 / 21:23:54 / 21:24:18 就是五连发）。
先按"攒 500 条 / 冷却 60 秒 / 安静 30 秒 / 上限 5 分钟"节流把平均压到 0.33% 总线，
再换成现在的实时增量，全量本身就不再是常驻成本。

**重建成本（只在整理/兜底时付）**：枚举 C 2.55s / 排序 0.15s / DFS 2.18s，
枚举 D 5.07s / 排序 0.29s / DFS 3.95s，落盘 0.9s，合计约 16 秒。已经做过三处
优化——DFS 共享一条 `pathBuf`（按层 append/截断，不再每条 `make([]byte)`）、
整盘根时跳过逐条 `underAny`、`pinyin.hasHan` 先扫 ASCII 字节再走 `unicode.Is`——
合计只省 6~8%，大头是 USN 枚举的 IO，单次成本砍不到几秒级。**这也是为什么解法
必须落在"不要那么频繁全量"上，而不是"把全量做快"。**

### 读增量：不等内核，自己 sleep

`READ_USN_JOURNAL_DATA.Timeout` 的单位是 **100 纳秒**。原代码把 `wait` 的秒数直接塞进去
（`5` 就是 500 纳秒），实测等于"无限等待"：卷上没有变化时 `DeviceIoControl` 会一直挂到
下次真有记录为止——观测到 D 卷单次挂 **4 分 47 秒**，两张卷串行读，C 卷的变化就跟着被
拖住。按 100 纳秒换算（5 秒 → `50000000`）后仍然整轮卡死：日志停更、进程 CPU 冻在
16.31 秒不动。

结论是内核这条等待路径不能用来限时。现在 `usn.Read(wait<=0)` 把 `Timeout` 和
`BytesToWaitFor` 都置 0——内核立即返回现有记录，等待交给调用方：`runIndexLoop` 每轮
读一次、然后 `time.Sleep(readPollInterval = 2s)`。超时行为完全可控，空闲时也不占 CPU。

主进程侧的交接同样是文件：服务每轮重建写 `index.bin`，每分钟写 `index.heartbeat`，
主进程 30 秒 stat 一次 mtime，变了就 `index.Load` 后 `idxReplace`（原子换指针，
正在进行的搜索不受影响）。

### 服务本身降优先级

`runIndexLoop` 一进来就 `SetPriorityClass(BELOW_NORMAL_PRIORITY_CLASS)`：重建那 15 秒
即使吃满一个核，也不会跟用户正在用的前台程序抢调度。

### 权限与开关

- 服务以 `LocalSystem` 跑（`sc create` 自动启动），只有它能打开 `\\.\C:` 读 USN；
  主进程始终是普通权限——这是服务化真正的收益。
- 安装/卸载在设置页「索引」分区，「启用快速索引（需管理员）」会 `ShellExecute
  "runas"` 拉起 `baozi-index.exe --install --data-dir <数据目录>`，弹一次 UAC。
  卸载同样走 `--uninstall`（停服务 → 等 Stopped → Delete）。
- **心跳在才用服务**。主进程不看"服务装没装"，只看 `index.heartbeat` 是否在 3 分钟内
  ——装了但停了（或服务崩了）就自动回退到自己扫，不会卡在等一个死的服务。
- 服务首次建索引期间 vs 之后内存差很大：建完必须 `debug.FreeOSMemory()`，
  否则构建期的 recs/排序/arena 会一直挂在 working set 上（实测残留 556 MB）。
  但只 `FreeOSMemory` 还不够——`Mutable` 握着那张 176 万条的**活堆表**，
  活对象回收不掉。2026-11 之后 `rebuild()` 落盘完就把堆表置空、换成
  `index.Load` 的 mmap 只读基座，服务端常驻从 313 MB 降到 108 MB（见 §15 第四轮）。

### 实测数字

| | WalkDir（主进程） | USN（服务） |
|---|---|---|
| 全量 174 万条 | 分钟级 | **16.8 秒**（只在首次、整理和兜底时做） |
| 运行权限 | 普通用户 | 服务（SYSTEM） |
| 常驻内存 | 主进程 280 MB | 主进程 280 MB + 服务 185 MB |
| 新建文件可见 | 下次重建 | **实时**（服务写 `index.delta`，面板唤出时追平） |
| 索引文件 | 23.4 MB | 20.0 MB（无 size/mod，压缩率更高） |

## 10. 已知边界

- 没有提权钩子：管理员权限窗口里的文件对话框暂时适配不到（见 §7/§8）。
- **Directory Opus / XYplorer / xplorer2 三种来源没有在真机上验证过**（用户自述这几样
  一个都没装）。窗口类名、协议、字段全按 Lertaro 的 collector 写；能验证的只有
  "没装时不会被误判、读不到时返回空不崩"。
- OneCommander 与 Files 不做：两者都要 UI Automation 才拿得到路径（见 §7 第 6 条）。
- tessoa 的目录来源是"问主实例"（`--print-cwd`），所以同时开两个 tessoa 时读到的是
  主实例的目录，可能不是当前前台那个窗口；每次查询还要起一个它的进程（热态 16ms），
  只在切到它和对话框弹出时各问一次，不在后台轮询。
- tessoa 停在欢迎页（`@Welcome`）时 `--print-cwd` 给的是盘根，这时跟随会跳到那个盘根。
- tessoa 若以**更高权限**运行（管理员）而 Baozi 是普通权限，`--print-cwd` 的转发可能被
  跨权限挡住：读不到就返回空、跟随器按"没读到"处理，不会乱跳也不会崩。两者权限一致时
  （都用普通权限，或都用管理员）没有这个问题。
- Directory Opus 的两个高级判法没做：本地化路径逐段还原（`LocalizedPathResolver`）、
  双栏按几何距离猜哪一栏是活动的。双栏判不出时宁可不动。
- 索引是实时更新的（服务把每条变化直接改进表，主进程面板唤出时追平 `index.delta`），
  但服务**停机期间**的变化要靠下次启动的全量才能补上；`rescanMinutes` 只在没有装服务时起作用。
- 服务常驻内存比原来高（62 MB → 约 185 MB）：现在它要一直握着整张表（35 MB Nodes +
  40 MB arena + 34 MB FRN 表）才能实时改，以前只是建完就扔、把 `index.bin` 当唯一真相。
  主进程也高了一些（对半是 copy-on-write 那一份：叠增量时复制 75 MB、改完原子换指针）。
- 路径查询（输入含 `\` 或 `/`）需要逐条还原路径，比纯名字查询慢，已限制候选上限 5000 条。
- 正则模式只匹配**文件名**，不匹配目录路径（普通模式那条"含 `\` 或 `/` 就走路径"的判定
  在正则里不成立，因为 `\.log$` 的反斜杠到处都是）；也没有"正则 + 拼音"的组合。
  正则语法错误只有结果区一行提示，不会自动帮你修模式。
- 正则模式是会话级状态（`application.regexOn`），不写 `config.json`：重启回到普通搜索。
- 缩略图只给 `.png/.jpg/.mp4/.pdf` 这类有意义的文件取；`.txt/.docx/.xlsx` 不取
  （它们的缩略图多半是白纸或空白页，还不如线框图标好认）。取不到时按线框显示，不会反复重试。
- 拖动排序只在首页「已固定」段有效，搜索态没有这个入口；拖动期间其它卡片不会让位，
  松手才整段重排（`Draggable` 的语义就是"元素跟指针走"，不做拖拽占位动画）。
- 缩略图缓存没有磁盘层，进程重启后要按系统 thumbcache 重新取（热态 1 ms/张，可接受）。
- 服务建出来的索引没有大小和时间（`SizeKB/ModSec=0`），按大小/时间排序用不了这两项。
- 非 NTFS 卷（U 盘 exFAT、网络盘）不能走 USN，配置里加这种根目录时服务会跳过它，
  主进程回退到 WalkDir。
- 互传是明文 HTTP + 一次性令牌，只适合可信局域网；大文件没有断点续传。
- 设置界面只覆盖常用项，`config.json` 里的冷门字段仍可手改；界面保存是整份覆盖，不保留手写注释。
- 内存：空索引时进程约 190MB（mygo 原生 UI + Go 运行时 + D3D），索引本身另算。

## 11. 图形化设置与热更新

第二个 mygo 窗口（920×820，`VibrancyGlass`），顶栏下按 `ui.Tabs` 分成两个页签：

| 页签 / 分区 | 控件 | 写回字段 |
|---|---|---|
| 常规·唤出 | 开关、`NumberInput`（间隔）、`TextInput`（备用组合键）、开关（全屏抑制） | `hotkey.*` |

| 常规·界面外观 | 三个 `Checkbox`（圆角 / 透明度 / 投影）、`Select` + `NumberInput`（图标大小：台阶百分比 / 自定义像素） | `appearance.*` / `search.iconScale` |
| 常规·文件对话框跟随 | 开关（总）、两行 `Checkbox`（七种目录来源：资源管理器 / Total Commander / Directory Opus / XYplorer / xplorer2 / tessoa / Arke） | `dialogFollow` / `pathSources.*` |
| 常规·排序 | 三条 `Slider`（频次/类型/准确性）、`TextArea`（类型优先级，每行一个扩展名）、`NumberInput`（最多结果）、`checkbox`（图片/视频缩略图） | `search.weights` / `typeOrder` / `maxResults` / `search.thumbnails` |
| 常规·索引 | 两个 `TextArea`（根目录、排除目录，每行一条）、开关（隐藏）、`NumberInput`（重建间隔）、按钮（立即重建） | `index.*` |
| 自定义命令 | 左列 `ui.List`（每条两行：显示名 + 全部激活词）＋ 新建/删除；右列表单：`TextInput`×3（激活词 / 显示名 / 程序）、`TextArea`（参数，一行一个）、`TextInput`（工作目录）、`Checkbox`（执行后不收起）、「将执行：」实时预览 | `commands`（结构化，不再是竖线文本） |

命令页的编辑口径（为什么这么设计）：

- **左边只负责选，右边才改内容**。以前一整个 `TextArea` 让人手拼 `关键字 | 显示名 | 程序 | 参数`，
  分隔符错一个整行就废，而且看不出哪条是哪条；现在一条命令就是一屏表单，用户只填不拼。
- **激活词一栏写多个**（逗号/顿号/竖线/空格都算分隔），第一个是主词、其余进 `aliases`；
  这样「关机 / shut / shutdown」是一条命令的三个叫法，不再是配置里三条重复的副本。
- **参数一行一个**，不用再考虑逗号分隔（旧格式里参数里出现逗号就没法写）。
- **`{dir}` / `%1…%9` / `%*`** 在 `cmd/baozi/command.go` 展开：有占位符 → 只按模板替换；
  没有占位符 → 回落老行为（把用户多敲的词追加到末尾），所以老命令的用法一点没变。
- 新建后没填程序/激活词的空壳**不落盘**（`save()` 里跳过），否则配置里会积一堆跑不起来的条目。
- 激活词被两条命令共用的，页顶给一行 `Warning` 提示（不拦保存）。
- 编辑器里"正在改的那条"用 `cmdSync` 记住上次同步自哪条：只有选中项真的换了才把文本搬进输入框，
  否则每帧都会把用户正在敲的字覆盖掉。
- **程序栏填 `.ps1` / `.bat` 自动带解释器**（`launchSpec`）：`.ps1` → `powershell.exe
  -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File <脚本>`，`.bat/.cmd` → `cmd.exe /c`。
  解释器用 `systemExe` 拼 `%SystemRoot%\System32\…` 的绝对路径，不走 PATH——Baozi 可能由 Run 键
  或托盘拉起，环境变量未必和手动开的终端一致，找不到解释器的表现就是"点了没反应"。
  设置页的「将执行：」用同一个 `launchSpec`，看到的和真正跑的一致。

「目录来源」是逐项写的 `true`/`false`（不是省略），否则用户关掉某一项之后，
下次读配置会因为"缺省即开"又给它打开。运行时的取法：

- **总开关**：`syncFollow()` 按 `dialogFollow` 起/停跟随器，改完保存立刻生效，不用重启进程；
- **来源勾选**：`application.sources` 是一份 `atomic.Pointer[map[string]bool]` 快照
  （配置指针在保存时整体替换，拿它给另一个 goroutine 每 200ms 读一次会数据竞争），
  跟随器每次轮询先问一遍，关掉的来源连窗口都不读。

「保存并应用」的执行顺序（顺序不能反）：

1. 写 `config.json`；
2. 组合键变了 → 先 `Unregister` 旧键再 `Register` 新键（直接覆盖注册会静默失败）；
3. 重启钩子进程：关掉它的 stdin 让它自杀，等 2s，仍在就 `Kill`，再重新拉起——钩子读的是同一份 `config.json`，不重启就还是旧间隔/旧全屏策略；
4. 根目录变了 → 触发一次重建（`index.Build`）；
5. `syncFollow()` 刷新来源快照（必要时起停跟随器）。

界面本身的可测性：`ui.NewTester(view, w, h)` 可以不开窗口就把视图渲染出来并断言文案，
所以面板、设置、互传三个界面都有快照式测试；键盘链路（双击 Ctrl → 输入 → Enter → 落盘）则用
`BAOZI_DEBUG=1` 配合模拟按键跑端到端。

踩过的坑，都写进了代码注释：

- 窗口创建后立刻 `Show()` 会被忽略（窗口存在但永远隐藏），必须延迟到事件循环里（`time.AfterFunc` + `panel.Update`）。
- 面板 `Show()` 之后还要 `Focus()` 窗口、且回车要在输入框上 `OnSubmit` 才可靠：
  `c.Shortcut(0, ui.KeyEnter)` 在焦点位于 `TextInputBase` 时收不到回车。
- **打包必须带 `-H=windowsgui`**：Go 在 Windows 上默认给 console 子系统，双击 exe 会先弹一个黑色终端窗口。
  换成 GUI 子系统后没有控制台，日志改写到 `%LOCALAPPDATA%\Baozi\baozi.log`（`dualWriter` 同时喂 stderr 和文件，
  stderr 写失败不能让文件写入跟着失败）。钩子进程仍是 console 子系统，但主程序用
  `SysProcAttr{HideWindow: true, CreationFlags: CREATE_NO_WINDOW}` 拉起，不会弹。
- **`OnClose` 里 `PreventDefault()` 会把整个退出取消**：mygo 的 `Application.Quit` 是
  `prepareQuit` → 逐个 `w.close()`，任意窗口的 `OnClose` 拒绝关闭就让 `prepareQuit` 返回 false、退出流程整体作废。
  我们的面板/设置/互传三个窗口都是"关闭=隐藏"，于是托盘「退出」表现为**日志写了收尾、钩子也退了，但主进程还在**。
  修法：`application.quitting`（`atomic.Bool`）在真正退出前置真，三个窗口的 `OnClose` 见到它为真就直接放行。
  另有 `BAOZI_AUTOQUIT=3s` 走同一条 `a.quit()` 路径，实测退出后 `baozi`/`baozi-hook` 两个进程都不残留。
- 托盘图标在 `OnWillQuit` 里 `Destroy()`，不要放 `OnQuit`：`prepareQuit` 末尾会 `stopNativeCalls()`，
  到 `OnQuit` 时 native 通道已经关了。
- **`SetForegroundWindow` 会被 Windows 前台锁拒绝**：mygo 的 `Window.Focus()` 内部就这一个调用。
  从别的应用或任务栏切回来时，我们的进程既不是前台进程、最近也没收到输入，于是
  `SetForegroundWindow` 只让任务栏闪一下，面板显示了但打字进不去（用户报的"点任务栏再双击 Ctrl 无法输入"）。
  修法在 `cmd/baozi/foreground_windows.go`：先 `AttachThreadInput(自己线程, 前台窗口线程, TRUE)`
  把两者输入队列接到一起，再 `SetForegroundWindow` + `BringWindowToTop` + `SetFocus`，最后成对脱离。
  焦点还要等窗口真正成为前台之后交给输入框（`panel.Update(func(){ input.Focus() })`），
  否则会被刚被激活的窗口抢回去。实测四种前台：记事本、任务栏（`Shell_TrayWnd`，无标题）、
  设置窗口、面板自己——都能唤出后直接打字。
- **新建窗口必须带 `Hidden: true`**：`mygo.NewWindow` 不带这一项就是创建即显示。设置窗口漏了这一个字段，
  结果 Baozi 一启动就弹一个 880×760 的设置窗口，占着任务栏、抢着前台——这也是"点任务栏后唤出不聚焦"的
  一半原因。互传窗口一开始就写了 `Hidden: true`，所以没暴露。
- 面板可见但已经不是前台时，`toggle()` 这一下应当**取回焦点**而不是收起：否则用户按了快捷键看到面板消失，
  还得再按一次。判断用 `panel.IsFocused()`。
- **`SHQueryUserNotificationState` 的状态从出参拿，返回值是 HRESULT**：
  `HRESULT SHQueryUserNotificationState(QUERY_USER_NOTIFICATION_STATE *pquns)`。
  之前按 `proc.Call()` 的返回值当状态读，那个值是 `E_INVALIDARG` 之类的错误码，永远不可能等于 2/3/4，
  等于这条判定从来没起过作用（D3D 独占全屏只能靠窗口矩形那条兜底）。
  现在传 `&quns` 并且只在 `hr == 0` 时采信。
- **全屏判定要把「最大化」排除掉**：这是用户报"启动后不弹面板 / 有时唤不出"的另一半原因。
  `D:\work\baozi` 这一版之前，判定是「窗口矩形覆盖整个显示器 + 没有 `WS_CAPTION`」。
  任务栏设成自动隐藏时，最大化窗口的矩形就等于整个显示器（甚至越过顶边 8px）；
  Total Commander 这类能关掉系统标题栏、自绘标题的程序 `WS_CAPTION` 也是 0，
  于是它明明只是个最大化窗口，却被判成全屏应用，`show()` 直接"忽略唤出"。
  修法是先 `IsZoomed(hw)`，最大化就返回——真正的全屏（浏览器 F11、游戏无边框全屏）用的是
  `SetWindowPos` 铺满屏幕，不是最大化状态，所以不会误伤。
  实测两种前台对照：无边框覆盖全屏的窗口 → `fullscreen=true`；Total Commander 最大化 → `fullscreen=false`。

## 12. 界面：从 Pane 借了什么

参考对象是 [Pane](https://github.com/LBEILC/Pane)（Windows 11 / WinUI 3 / .NET 10 的磨砂玻璃效率工具）。
它和 Baozi 的重合面只有「唤出 + 搜索 + 结果列表」这一段，所以只借**这一段的视觉与交互**：

| Pane 的做法 | Baozi 的落点 |
|---|---|
| 首页是图标网格，「最近使用」+「已固定」两段，不是空白也不是纯列表 | `application.homeGrid()`：自己拼行（卡片 84 + 间距 6，一排几个由 `gridCols()` 按面板宽度算，默认 720 → 7），不用 `ui.Grid`——Grid 均分列宽会让卡片左右各空一截；`pinnedCount` 记录 results 里属于「已固定」的后缀长度 |
| 选中应用 `Ctrl+K` 固定到首页 | `application.togglePin()` + `history.Store.TogglePin()`；标记存 `Entry.Pinned`，`Pinned(n)` 取固定项，`trim()` 优先保留固定项 |
| 底栏左边写「会做什么」，右边写键位 | `footer()` + `keyCap()`：左边 `打开 抖音` / `已固定 · 记事本`，右边几组键帽 |
| 底栏分两层：键帽只提示，入口是按钮 | 键帽是"按了会怎样"的说明，故意画轻（`Surface` 底、无边框、10 号 `TextMuted`）；「设置」「互传」是能点的入口，用 `ui.Button`（白底 + `Border` 边 + 深色字，自带悬停/按下态），中间空开一截分组。早先两者同一套浅灰方块，看上去完全一样——这是反例，别再混回去 |
| 选中行是「浅色胶囊 + 细边」 | `row()` 用 `Accent.Alpha(0.16)` 底 + `Accent.Alpha(0.5)` 1px 边；名称 14 粗、路径 11 灰 |
| 顶栏放大镜 + 右上品牌胶囊 | 同一个 `SearchIcon` SVG + 右端「Baozi」胶囊 |
| `Ctrl+Enter` 打开所在文件夹 | `application.openFolder()`：文件开父目录，目录开自身，`shell:` 路径直接跳过 |

**搜索框右边的 `.*` 按钮**（`application.regexButton()`）：输入框与品牌胶囊之间一个
`ui.Button(c, ".*")`，开着时 `Accent.Alpha(0.28)` 底 + `Accent.Alpha(0.6)` 细边把状态标出来；
关着是 `Surface` 底、`TextMuted` 字。点一下改 `a.regexOn` 并 `refilter()`，
同时把底栏状态写成「正则模式：只精确匹配文件名」——正则和普通搜索的结果差得很远，
状态不显式说明，用户会以为搜索坏了。占位提示也跟着换成
`正则匹配文件名，如 ^2026.*\.log$`。

**为什么要自己画选中态**：mygo 的 `ui.ListState.Selected` 一旦设置，List 会用整块 accent 填充画选中行
（还压在行自己的背景上）。要让「浅色胶囊 + 细边」成立，就把 `Selected` 留空（`a.list.Selected = nil`），
选中由 `row()` 自己画，滚动仍然靠 `choose()` 里的 `ScrollIntoView`。

**首页方向键按行移动**：网格一行几个由 `gridCols()` 决定（默认面板 720 宽时是 7），
`↑`/`↓` 走 `±gridCols()`，`←`/`→` 走 `±1`；
有查询时回到单列语义（`±1`）。这一条写在 `view()` 的 `home := a.query == ""` 分支里。

**没有抄的部分**：剪贴板历史、鼠标手势、超级面板、AI、批量重命名、定时关机、`Tab` 展开的三栏文件窗口
（左分类/中结果/右预览）、`Alt+数字` 动作排。前四类与 Baozi 的定位不重合；三栏文件窗口依赖
「读第三方文件管理器当前目录」（§7）尚未实现；动作排需要一套「对任意文字的动作注册表」，
现在只有「打开 / 打开文件夹 / 固定」三个确定动作，做成键帽更诚实。

**可测性**：界面仍用 `ui.NewTester` 在内存里渲染，新增三条行为测试——
`TestHomeGridSplitsPinned`（固定项进「已固定」段）、`TestCtrlKPinsFromKeyboard`（焦点在输入框时 `Ctrl+K` 也生效）、
`TestHomeGridArrowMovesByRow`（`↓` 走一行）。
注意 Tester 环境**没有 native 消息循环**，`icon()` 里的 `Shell.FileIcon` 会一直阻塞，
所以这类测试要先 `stubAllIcons()` 把图标缓存填上纯色位图。
同理，`mygo.App.OpenAtLogin` 走的是 `onMainValue`，在 Tester 下没有主循环也会永久阻塞——
自启状态因此在 `ready()` 里问系统一次存进 `application.openAtLogin`，设置窗口和托盘只读缓存。

### 内容缩略图（`internal/win/thumb.go`）

结果行左侧对 `.png/.jpg/.mp4/.pdf` 这类文件显示的是**内容缩略图**，不是扩展名图标。
两条限制叠在一起，才逼出这个自实现的 `win.Thumbnail`：

1. **mygo 的 `Shell.FileIcon` 拿不到缩略图**。它内部固定传 `SIIGBF_ICONONLY`（源码注释原话
   "as FileIcon draws its icon, without thumbnails"），所以哪怕放开 `isProgram` 的判断，
   png 也只会拿到同一枚蓝色"图片类型"图标。要看内容必须自己调
   `IShellItemImageFactory::GetImage`，且 flags 不带 `ICONONLY`。
2. **成本差一个量级**。同一个函数按两种 flags 各取一次（`tools/thumbprobe`，28 张系统壁纸、48px）：

   | 场景 | 每张 | 首屏 10 张 | 全部 80 条 |
   |---|---|---|---|
   | 类型图标（`ICONONLY`） | 平均 2.0 ms（首张 26 ms） | ~20 ms | ~110 ms |
   | 缩略图 · 冷 | 平均 19.1 ms，最慢 80 ms | ~190 ms | ~1.5 s |
   | 缩略图 · 热（系统 thumbcache 已建） | 平均 1.1 ms | ~11 ms | ~90 ms |

   冷态同步取一屏就是几百毫秒白屏。所以**搜索完全不碰图标**（不受影响），要控的只是渲染：
   渲染时只"下单"，图回来再 `panel.Update` 重绘，第一帧仍是线框图标。

实现要点：

- `Thumbnail(path, px)` 自己组 COM 调用：`SHCreateItemFromParsingName` +
  `IShellItemImageFactory::GetImage(SIIGBF_CROPTOSQUARE|BIGGERSIZEOK)`。
  `CROPTOSQUARE` 是因为照片是 4:3 的，按比例缩进方块会拉变形；`BIGGERSIZEOK` 让 Shell
  从更大的缓存图缩下来，22 逻辑 px 才不糊。
- **COM 的 STA 与线程绑定**，而这个函数跑在后台 goroutine 里：进去先 `runtime.LockOSThread()`，
  `CoInitializeEx(COINIT_APARTMENTTHREADED)` 返回 `S_OK`/`S_FALSE` 时配对 `CoUninitialize`；
  `RPC_E_CHANGED_MODE` 说明线程已被别人初始化，不要动它。
- 返回值是 `HBITMAP`，用 `GetObjectW` 取 `DIBSECTION` 直接读 `bmBits`（32 位 BGRA + 直通 alpha），
  转 `image.NRGBA` 再 `png.Encode`，交给 `ui.DecodeBitmap`。
- **并发与去重**：`thumbSem` 容量 4（Shell 那边是 COM 走 explorer 的缩略图提供者，
  放几十个进去只会互相拖慢）；`thumbAsk` 记住请求过的 key（含失败的），避免每帧重复下单。
- **缓存**：`icons` 复用应用图标那张 map，key 是 `thumb:<px>:<path>`。
  成功的存位图、失败的也存（值为 nil），这样"系统给不出缩略图"的文件不会每次滚动都重试。
  超过 900 条就整批丢掉缩略图部分——重建一张只要 1 ms，不值得维护 LRU。
- **`ui.List` 是虚拟列表**（只有可见行调 `row()`），所以"只给可见行取图"不需要额外判断。
- 开关：`search.thumbnails`（`*bool`，nil 视为开），设置界面的「排序」区有勾选。
  关掉后一律线框图标。

### 图标多大（`search.iconScale` + `application.iconSize`）

结果行左边那个图标，尺寸过去是写死的 22 DIP。22 在 720 宽的面板里占 3%，在 125% 缩放的屏上
（这台机器就是 120 DPI = 125%）看着偏小——虽然 mygo 是 PerMonitorV2 DPI-aware、22 DIP 已经
被系统渲染成 27.5 物理像素，但**相对面板的比例没变**，用户看到的还是"小"。

**单位是倍数，不是像素**（用户的原话："为什么设定的是 px，改为倍数"）。像素说法有个绕不过去的
问题：换台显示器、改一下系统缩放，同一个数字看起来就不一样了，用户没法表达"我要比默认大一圈"。

- `config.BaseIconSize = 22` 是 100% 缩放下的基数；
- `search.iconScale` 是倍数（1 = 100%），`IconSizeAt` 算 `BaseIconSize × 系统缩放 × 倍数`。
  于是 100% 在任何屏上都是"这块屏的默认样子"，125% 屏上是 27.5、150% 是 33；
- 倍数夹在 `MinIconScale = 0.5` ~ `MaxIconScale = 3`：再小认不出图标，再大格子撑爆面板；
- 设置页在**「界面外观」**里给这一项（它改的是面板长什么样，不是搜什么、怎么排）；
  下拉给 75/100/125/150/175/200% 六个台阶，选「自定义」才露出手输框；
- **自定义那格填的是像素，不是倍数**（用户的原话："自定义直接改为设置px大小"）。
  台阶按百分比留着，因为"默认多大"本身随系统缩放走，只有百分比是稳的刻度；而自定义
  反过来只认像素 —— 用户盯着面板上的图标，能说出口的是"33 像素"，不是"1.2 倍"。
  输入框的上下限就是 `BaseIconSize × 系统缩放 × 0.5 ~ 3`（125% 屏上是 13.75 ~ 82.5），
  填进去的数当场折成倍数（`iconScale = 像素 ÷ 基准`），落盘的仍是 `iconScale`。
  旁边那行灰色读数（`iconPreview`）跟输入框是同一个数，所以填 33 就显示 33.0 像素；
- **老配置兼容**：`search.iconSize`（像素）字段留着，`IconSizeAt` 的优先级是
  `iconScale → iconSize → 跟随系统缩放`。老用户升级时看到的尺寸不会跳一下；
  一旦在设置页保存过，就写 `iconScale` 并把 `iconSize` 清零（两套值同时存在没人说得清信哪个）。

`application.iconSize()` 只做一件事：`a.cfg.IconSizeAt(a.scale)`。`a.scale` 是 `ready()` 里
`mygo.Screen.Displays()` 取到的最大缩放（App 级 API，在 `ui.Tester` 下会阻塞，所以只算一次缓存）。

两个跟尺寸耦合的地方要一起改，否则会出现"设置生效了但布局错位"：

1. **首页卡片**（`cardIcon` / `cellWidth`）：图标按"结果行图标 ×2.2"放大，卡片宽度
   `cellWidth() = max(86, 图标 + 16)`——**卡片跟着图标变宽**，不再是一个固定 86。
   早先卡片写死 84 宽、图标夹在 64 以内，倍数一调大图标就会顶出底块；现在宽度、列数
   （`gridCols`）、拖动换位的步长（`pinMoveFor.colStep`）全部从 `cellWidth()` 来，三者同源，
   所以"图标放大 → 一排少放几张"是自然结果，不是裁掉。
   图标只夹下限 32：上限不夹，因为夹了就等于把"倍数"这个说法废掉一半。
   卡片高度是"上下 padding 8 + 图标 + 6 + 两行字 + 行距 6"，`pinMoveFor` 用
   `icon + 49` 跟着算，不然拖动重排会跳格。
2. **结果行行高**：行高是"图标"和"名字 + 提示两行字"取高的那个，而名字那列本身就有 38 左右。
   所以 22 → 27.5 时**行高不变、显示行数也不变，只是图标变大**；只有图标超过 38 才会把行撑开。
   这不是 bug，是想要的：默认换大一号不该让一屏少显示两行。`TestIconSizeSettingGrowsRows`
   把这两段都钉住了（44 时行高几乎不动，60 时才明显涨），`TestIconScaleWidensCards`
   钉住"倍数放大 → 卡片变宽 → 一排变少"这条连锁。

**卡片里图标只有程序是彩色的**：`KindApp` 与 `.exe/.lnk/.msi/...` 走 `appIcon`（系统彩色图标），
目录/文档/命令仍是 `icon.go` 的单色线框（那是 Pane 式的统一语言，也是当初换掉系统文件图标的
理由：白纸片在深色玻璃底上刺眼、同排花花绿绿不成体系）。所以首页如果是纯应用/游戏，
看到的就是一整排"大彩色图标"；混进文档时线框会显得轻一些，这是有意的区分。

实拍：`BAOZI_SCALE=1.25 BAOZI_PREVIEW=… go test -run TestIconPreviewShot`，这张图就是
125% 屏上结果行的默认样子；`BAOZI_SCALE=1` 再跑一次即得 100% 的对照。
卡片网格另有 `TestCardIconPreview`：它用 `win.Thumbnail` 取真实 `.exe` 图标塞进缓存再渲染
（`appIcon` 现在先查缓存再问 Shell，所以离线也能命中），跑出来的就是真机上首页的样子。
`BAOZI_PANEL_W` / `BAOZI_PANEL_H` 可以在预览里换面板尺寸，顺便验证列数跟着变。

### 图标为什么曾经是"白框里一个小图标"（`win.TrimIcon`）

用户报过：首页卡片上「遗忘之海」显示成一个方形浅灰细框 + 白底 + 中间一个小 logo，
而游戏启动器里它是满格的彩色图标。`win.Thumbnail` 拿这个 exe 的图标一验，**位图本身就长这样**。

拆开看那张 200×200 的 PNG：

- 四角 `alpha` 只有 58~64（约 23%~25%），是 Windows tile 图标自带的一圈**淡阴影**；
- 中间 logo 区 `alpha` 是 255；
- 两者之间是 `(0,0,0,0)` 的纯透明。

旧的裁边只按 `alpha > 1.6%` 找内容边界。那圈 25% 的淡阴影跨过了门槛，于是 bbox 等于整张图，
**一个像素都裁不掉**——图形只占中间一小块，显示出来就是"白框小图标"。`flatCut` 单独也救不了：
阴影不是纯色平板，每行颜色都在渐变。

修法是两级门槛：

1. `alphaCut` 先按 `solidAlpha = 30%`（`0x5000`）找"实心内容"的外接框——这刚好跨过那圈淡阴影；
2. 实心区找不到（半透明描边图标）才退回老的 1.6% 门槛；
3. 之后再叠 `flatCut`：从四边往里裁"整行/整列颜色一致"的不透明平板边（真正的白底/灰 tile 边框）。
   判据是**行内自洽**而不是"等于角落色"，这样"白底 + 浅灰边框"两层能逐层剥掉；
   两条保护：整张几乎一个颜色（纯色方块图标）就原样返回，裁到剩不到 15% 也当作裁过头退回。

实测 `PCLauncher.exe`：裁前 8149 字节（白框小图）→ 裁后 6549 字节，图形铺满整格。
单测：`TestTrimIconCentersContent`（透明边 + 居中）、`TestTrimIconTrimsOpaqueTile`（128 的白底+灰框被裁到 logo 大小）、
`TestTrimIconKeepsSolidSquare`（纯色方块不许被裁小）、`TestTrimIconRejectsBlank`。

配套还有一条：历史里出现过 `shell:AppsFolder\D:\Games\x\PCLauncher.exe` 这种**畸形 Path**
（前缀后面跟的是真路径，不是 AUMID）。`iconPath()` 会把这类剥成普通路径再取图标，
其余 `shell:AppsFolder\<AUMID>` 原样交给 Shell——按扩展名当普通文件取只会拿到一个无关的通用图标。

### 面板多大（`panel.width` / `panel.height`）

面板尺寸过去是 `main.go` 里的两个常量（720×480），`WindowOptions.DisableResize: true`。
4K 屏上嫌小、笔记本竖屏上嫌宽，而且首页一排固定 7 张卡，宽窄都不跟着变。

- `config.Panel{Width, Height}`（json `panel`），0 或缺省 = 默认 720×480；
- `Config.PanelSize()` 夹到 420~2400 / 260~1600，越界的数存盘前就夹好，配置文件里不会躺一个永远不生效的值；
- **没有设置项**：用户直接拖面板的四条边（`TitleBarHidden` + `HideWindowButtons` 才有 Windows 的缩放边缘，
  见 `ready()`）。拖动中 `panel.Size()` 实时变，`gridCols()` 直接问它，列数跟着重排；
- 停手 0.8 秒后 `panelResized()` 攒下的尺寸由 `savePanelSize()` 写进 `config.json`（尺寸没变就不写，
  否则每次唤出的 `SetSize`/`SetPosition` 都会触发一次无谓的落盘）；
- 不用 mygo 的 `StateKey`：它只在窗口真正 close 时写盘，而 Baozi 的"关闭"只是 `PreventDefault` 隐藏；
- **列数不再是常量**：`application.gridCols()` = `(宽 - 2*cardGutter + cardGap) / (cardWidth + cardGap)`，
  720 → 7 列、1200 → 13 列、480 → 5 列。`homeGrid()` 的排布、`pinMoveFor()` 的行列换算、
  `↑`/`↓` 的步长全部走它，三者必须同源，否则"一排看得到 13 个、方向键却跳 7 格"。
- `panelCols` 常量只留作"默认宽度下的列数"，给测试和默认布局当基准。

设置页右侧实时算给用户看："一排 7 列、约 10 行；物理 900×600"（物理值乘系统缩放）。
`TestGridColsFollowsPanelWidth`、`TestPanelSize` 钉住换算与夹取。

### 面板摆在哪（`panel.x` / `panel.y`）

面板没有标题栏（`TitleBarHidden`），原本完全拿不动，位置也永远是"鼠标所在那块屏居中偏上"。
现在能拖、并且记住位置：

- **拖**：`view()` 的根 `ui.Column(c).Fill()` 上挂 `.DragWindow()`。mygo 的命中测试是
  "子元素优先"，所以输入框、卡片、按钮照常收事件，只有点在空白处才是拖窗口
  （`ui/input.go` 里 `startDrag()` 那条路径）。实测按下 830,570 拖到 1030,700，窗口整块跟手。
- **记**：`panel.x` / `panel.y` 是 `*int`——`nil` 是"从没拖过"（走默认居中偏上），
  `0` 是"真的记在原点那块屏"。用 `int` 加哨兵值在 0,0 上会打架。
- **存**：`OnResize` 和 `OnMove` 都进同一个 `panelGeometryChanged()`，共用同一个 800 ms 定时器
  → `savePanelGeometry()`。两个回调各写一份 `config.json` 会互相覆盖（尺寸写进去、位置被旧值顶掉）。
- **两个必须挡住的假信号**（都是实测撞出来的）：
  1. 面板弹出后的头 900 ms 里，位置会被系统/mygo 挪几下（一次性连续 `OnMove`，配好的位置能被一路
     滑走几十 DIP）。这段时间的位移不记，`panelSettle` 之后才当真；
  2. 面板**隐藏之后**还会被挪（实测隐藏 5 秒后位置漂到 506,354）。`!IsVisible()` 直接丢弃，
     否则每次唤出都把记录改写成"被挪到的那个值"，复现永远对不上——真机验证时就是靠这两条
     才从"每次启动位置都不同"变成"重启后精确回到 800,504"。
- **摆回去**：`show()` → `panelPosition()`：有记录就用记录，没记录就居中偏上。两条路径都过
  `clampPanelTo()` 夹进"最近显示器"的工作区（面板中心用来找最近的显示器）——拔副屏、改分辨率
  之后旧坐标可能整块在屏外，夹回来才不至于"唤出后面板看不见"。存盘前也夹一次，免得配置里
  躺着一个下次必然被改写的值。
- `show()` 里 `placePanel()` 会摆一次、450 ms 后再补一次：面板刚显示时位置确实会被改掉。
  `TestPanelPos`（`*int` 语义）与 `TestClampPanelTo`（区内不动 / 越界拉回 / 面板比工作区还大
  时贴左上）钉住这两块。

### 已固定区的拖动排序

首页「已固定」的卡片可以拖动重排，松手即保存。三个不能省的细节：

- **不能在卡片渲染途中改 `results`**：那一帧还在遍历它，改了就越界。
  松手时只记下 `pendingPinMove{from,to}`，下一帧 `view()` 开头的 `applyPending()` 统一结算
  （和右键菜单的"固定/删除"走同一条通道）。
- **`Draggable` 的 offset 指针必须跨帧稳定**：`pinDragPts map[int]*ui.Point` 按卡片缓存指针，
  每帧新建一个 `Point` 会把拖动中的卡片拽回原位。结算前要 `*pt = ui.Point{}` 归零，
  否则卡片会粘在拖到的位置；重排后 `pinDragPts` 整表清空，避免旧下标对上新的卡片。
- **位移怎么换算成落点**：一列 = 卡片 84 + 间距 6 = `cardWidth + cardGap`（90 DIP），
  一行 = `cardIcon() + 49`（图标 + 上下 padding + 间距 + 两行字 + 行距，默认约 89）。
  `round(dx/90)` 加 `round(dy/行高) * gridCols()` 就是"挪了几格"，再夹到 `[0, pinnedCount-1]`。
  这几个数字必须和 `card()`、`homeGrid()` 保持一致，改布局时一起改。
- **窗口拖动必须让开**：面板的自绘拖动挂在 `MyGoSurface` 的子类化上，比 mygo 的 UI
  先收到鼠标移动；不管按下的是卡片还是空白，位移一过 6px 就会被它接管，表现成
  "拖卡片时整块面板跟着走"。所以 `wmLButtonDown` 里先用 `pinPressHit()` 反查固定卡
  矩形，命中就把 `pending` 清掉、消息照旧转发给 mygo。
- **固定卡矩形用 `ui.Handle` 取**：`Element` 跨帧会失效；`card()` 里给每张固定卡
  `Bind` 一个跨帧 `Handle`，再读 `Bounds(c)`（上一帧提交后的矩形），存进 `pinRects`。
  客户区消息是物理像素、矩形是 DIP，比较前要按 `GetDpiForWindow` 的缩放除回去。
- **拖动中的卡片要压在别的卡片上**：只设被拖卡片的 `ZIndex` 只能压住同一行；
  跨行拖时后面的行按文档顺序画，会把卡片盖住。所以被拖卡片所在的行也要 `ZIndex(1)`。

`Draggable` 的语义是"元素跟着指针走、松手才停"，所以拖动期间其它卡片不会让位——
出现的是"被拖的卡片盖住目标位"的观感，松手后整段重排。测试 `TestPinDragReorders` 用
`ui.Tester` 的 `Press/Move/Release` 真跑一遍：往右拖一格，「甲甲」从第一位变成第二位，
且 `hist.Pinned` 的顺序在重开后依然是新顺序。

## 13. 启动方式与图标资源

**开机自启**用 mygo 的 `App.SetOpenAtLogin`，Windows 上就是写用户的 Run 键：
`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，值名 `Baozi`，
内容 `"<绝对路径>" --mygo-opened-at-login`。选 Run 键而不是计划任务，是因为 Baozi 不需要提权；
需要管理员权限的程序才应该用「最高可用权限」的登录任务。

`--mygo-opened-at-login` 是"被系统拉起来的"标记，mygo 在 `init` 里读 `os.Args` 后会把它剥掉，
主程序只通过 `App.WasOpenedAtLogin()` 看到布尔值。`ready()` 里据此决定首帧要不要 `show()`：
自启就只建托盘 + 钩子进程待在后台。状态不存 `config.json`——系统的 Run 键才是唯一事实，
用户可能在任务管理器里禁用，存一份副本必然两边打架。

**图标**：同一个包子形象（暖橙圆角底 + 奶油白包子 + 顶部三褶），三处复用：

| 落点 | 载体 | 机制 |
|---|---|---|
| 托盘/通知栏 | `cmd/baozi/assets/tray.png`（32×32） | `go:embed` 进 `trayPNG`，`TrayOptions.Icon` 要的就是 PNG |
| exe 图标 | `cmd/baozi/assets/baozi.ico`（7 个尺寸） | 由 `rsrc` 编成 `rsrc_windows_amd64.syso`（COFF 资源），`go build` 自动链接 |
| 窗口图标 | 无单独设置 | mygo 的 `WindowOptions` 没有 Icon 字段，窗口图标直接取 exe 的资源，所以上一条改完窗口/任务栏/Alt+Tab 一起变 |

为什么要 syso 而不能只放旁边一个 ico：PE 的图标在 `.rsrc` 节里，Windows 读的是那一段，
旁边放文件只会被忽略。`rsrc` 本地没有二进制时用 `go run github.com/akavel/rsrc@v0.10.2`。

为什么要程序化画：托盘 16px、任务栏 32px、资源管理器大图标 256px，同一张位图缩到 16px 会糊；
`tools/icongen` 对每个尺寸按解析式重画（4×4 超采样、边缘 1px 抗锯齿），小尺寸自动省掉褶子。

**开发时怎么重启才不会被"连带杀掉"**：`Start-Process` 起的进程是当前 shell 的子进程，shell 又是
宿主（终端 / 编辑器 / IDE 插件）的子进程；宿主退出时 Windows 会把它 Job 对象里的整棵进程树一起
收走，于是"关掉宿主，Baozi 也没了"。`tools/restart.ps1` 走 `Win32_Process.Create`（
`Invoke-CimMethod -ClassName Win32_Process -MethodName Create`），由 WMI 服务去 CreateProcess，
新进程的父进程是 `WmiPrvSE.exe`，和终端没有父子关系，Job 也够不着它。脚本顺带收掉旧的
`baozi` / `baozi-hook`、等 2 秒、启动后打印父进程名以便确认真的脱离了宿主。产品本身的启动方式
（Run 键自启、双击 exe）不受影响——这两种都不在谁的 Job 里。

## 14. 卸载

入口在托盘「卸载：清除自启与数据…」，范围是"摘自启 + 删数据目录"，**exe 保留**（`cmd/baozi/uninstall.go`）。
先走 `mygo.Dialog.Message`（`MessageWarning`、按钮 `取消 / 卸载`、`DefaultButton: 0`、
`CancelButton: 0`、`DestructiveButtons: [1]`、`Detail` 里逐条列出会删什么），`res.Button != 1` 就当取消。
调用方用 `go a.uninstall()`：模态对话框不该占着主线程的消息循环。

确认后的顺序（顺序不能反）：

```
SetOpenAtLogin(false)  → history.Close() → share.stop() → uninstalling=true → quitting=true
→ shutdown()（关钩子、取消扫描、跳过历史落盘）→ 等扫描 goroutine 结束
→ closeLog()（放开日志句柄）→ os.RemoveAll(dataDir) → App.Quit()
```

几个必须这样排的理由：

- **`history.Close()` 和 `uninstalling` 标记都是为了"删完又长回来"**。历史 `Touch` 后有 400ms
  防抖落盘，最后一笔可能在目录删掉之后才触发；`Save` 里有 `os.MkdirAll(filepath.Dir(path))`，
  于是 `history.json` 带着目录一起被重建。`Close()` 停定时器并让之后的 `Save` 变空操作；
  `shutdown()` 里也加了 `!a.uninstalling.Load()` 才 `Flush`。
- **`closeLog()` 必须在 `RemoveAll` 之前**：Windows 不允许删除正在写入的文件，句柄不放开会删一半。
  `logFile` 存的是 `setupLog()` 打开的那个句柄，关完把 `log.SetOutput` 换成 stderr（GUI 下等于丢弃，反正马上退）。
- **要等后台扫描结束**：`shutdown()` 的 `scanOff()` 只是取消 context，扫描 goroutine 可能正处在落盘阶段。
  现在 `rescan()` 有 `scanWG.Add/Done`，卸载用 `waitScan` 等它（上限 5s）。
- **顺带修正的真 bug**：`rescan()` 原来在 `ctx.Err() != nil` 时仍会 `index.Save` 已扫到的部分结果。
  这不只影响卸载——用户点「重新建立索引」又立刻退出，也会留下半份 `index.gob`，
  下次启动当完整索引加载，搜索结果就缺东西。现在取消即返回，不落盘。
- `config.Dir()` 与 `history.Save()` 都有 `MkdirAll`，所以卸载流程里 `RemoveAll` 之后不能再有任何
  触碰它们的调用。`quitting` 置真让三个窗口的 `OnClose` 放行，`App.Quit` 才能正常关窗退出。

验证方式（隔离，不动真实数据）：`BAOZI_DATA_DIR=<临时目录> BAOZI_HEADLESS=1 BAOZI_UNINSTALL=1`，
实测结果是临时目录整个消失、`Run` 里没有 `Baozi`、`baozi`/`baozi-hook` 两个进程都退出。
真实数据目录（含 179 万条索引）未被这一路测试碰到。

## 15. 内存：从 526 MB 压到 173 MB

用户报"占用了 500 多 M，太多了"。先把事实量出来，再谈怎么改。

### 诊断

`tools/memprobe`（`//go:build ignore`）加载真实索引后逐段打印：`Node` 结构 56 字节、
1,791,246 条、结构本体 95.7 MB、名字内容 40.6 MB（平均 23.8 B/条）、
`Py` 只有 9,228 条（0.1 MB，却给每条留了 16 字节字符串头）、
模拟 `catalog.lowers` 再加 39 MB。

pprof（`BAOZI_PPROF=1`）`-inuse_space` 抓到 `reflect.growslice` 86 MB、
`internal/saferio.ReadData` 74 MB、`encoding/gob.decString` 25 MB，
`index.Load` cum 185 MB（占全部活数据 95.8%）——索引几乎就是全部，gob 解码另产 160 MB 垃圾。

`BAOZI_MEMSTAT=8s` 的曲线（`heap=456MB inuse=459 sys=496 objs=2092272 gc=8`）说明 GC 根本没被触发：
活数据大 + 默认 GOGC=100。

### 第一轮：GC 策略 + 归还空闲页

- `main()` 里 `debug.SetGCPercent(40)`：让 GC 更早介入，代价是 CPU；
- 新增 `releaseMemory()`：命中快照不扫描时、扫描结束后各调一次，
  延迟 2s 再 `debug.FreeOSMemory()`（别让 STW 卡首帧）。

结果：`Private 526 → 369 MB`、`WS 490 → 326 MB`、`heap 457 → 209 MB`。但索引本身仍是 185 MB。

### 第二轮：改存储格式（v3）

三件事一起做，因为它们的收益互相独立：

1. **`Node` 56 → 20 字节**：去掉 `Name string`（改偏移+长度）、去掉 `Py string`
   （改 `map[int32]uint64` 打包偏移，只有 9 千条）、`Size int64` → `int32 KiB`、
   `Mod int64` → `int32`（相对 2020-01-01，够用到 2088 年）。
   `179 万 × 36 字节 = 省 64 MB`。
2. **名字进 arena**：名字不再每条一个字符串对象，而是 `Names []byte` 里连续的一段。
   省的其实不是内容（40.6 MB 不变），是 179 万个小对象的头和按 size class 取整的浪费。
3. **去掉 `catalog.lowers`**：预存小写表 39 MB。改成在 arena 字节上直接做大小写不敏感比较
   （`containsFold`：ASCII 折大小写，汉字字节 >0x7f 原样比）。

顺带把 `gob` 换成定长记录 + arena + gzip：加载变成"一次解压 + `unsafe.Slice`"，
零解码、零拷贝，也不再产生 growslice / ReadData 那两笔峰值。

引擎编号 `storeVersion = 3`，文件名从 `index.gob` 改成 `index.bin`（旧文件不会自动删，
它已经不被读取，可以手工清掉）。

### 第三轮：mmap + 只读基座 / overlay 增量（v4）

第二轮之后还有两笔没处理掉，都是 pprof 抓出来的（157 MB 进程里三份表同时驻留）：
`index.Load` 73.76 + `(*Table).ApplyOps` 42.09 + `(*Table).Clone` 41.02。

1. **v3 的"零拷贝"是假的**：`Load` 把 gzip 解出的 55 MB 缓冲用 `unsafe.Slice` 指进
   `Nodes/Names`，看着没复制，但那块缓冲**是 Go 堆上的匿名内存**：全量常驻、页还回不去，
   而且 `Clone()` 里的 `raw` 共享让每一代快照都引用它，永不回收。
   → v4 改成**未压缩 + MapViewOfFile**：同样是"指针指向数据"，但数据在文件的页缓存里，
   file-backed、可丢弃。堆占用与条目数彻底脱钩。
2. **增量的 copy-on-write 太贵**：`Clone()` 复制一份可写表（76 MB ≈ 15 ms），30 秒一次，
   实测让 WS 反复多挂 100 MB，Private 顶上 331 MB。
   → v4 改成**只读基座 + overlay**（新条目进 `ovNodes/ovNames`，删除进 `dead` 集合），
   `Clone()` 只复制那个几十 KB 的尾巴。

顺带把搜索预筛也压了一层：`Hits` 先比 `NameLen`（名字比词短直接跳过），
再在 arena 字节上做零分配的 `containsFold`。这是"位图预过滤"的廉价替代——
179 万条里绝大多数名字和查询词长度都差得远，挡在最前面比查位图还便宜。

配套还有三处，单独看都很小、加起来才有观感：`Clone()` 不再共享 `raw` 并预分配、
增量成功后 `releaseMemory()`、`show/toggle` 隐藏分支 `releaseMemory()`。

| 指标 | 第二轮（v3） | 第三轮（v4） |
|---|---|---|
| Go heap | 86 MB | **8 MB** |
| sys | 255 MB | **16 MB** |
| Private | 251 MB | **173 MB** |
| WorkingSet | 212 MB | 151 MB |
| 索引文件 | 19.3 MB（gzip） | 73.8 MB（未压缩） |
| 增量一次的代价 | 复制 76 MB 整表 | 追加几十字节 |

**mmap 带来的两个新坑**（细节见 §3 落盘）：

- **被映射的文件不能替换**：服务端重建后 `Rename(tmp, index.bin)` 会 `Access is denied`，
  因为主进程正映射着上一代。改成"代次文件"（`index-<时间戳>.bin`）+ 两边按 mtime 挑最新，
  加载完 `Prune` 收掉更老的。这个错误第一次出现时是静默的——服务端日志里只有一行
  `首轮建索引失败: 保存索引: rename ... Access is denied.`，索引就停在上一代了。
- **映射必须跟着 Table 活**：`Clone` 出来的表共享同一份映射，所以是引用计数 + finalizer，
  不能"换表就关"——搜索线程可能还拿着旧快照在遍历。

### 第四轮：索引服务落盘后切回 mmap 基座

前三轮改的都是主进程。`baozi-index` 服务进程一直占 **WS 292.8 MB / 私有 313.7 MB**，
而同一时刻 vmmap 显示它 `MAPPED` 只有 1.2 MB、`PRIVATE` 311.6 MB——它压根没走 mmap。

病根在 `cmd/baozi-index/main.go` 的 `rebuild()`：`usn.BuildAll` 返回的 `*index.Table`
被 `index.NewMutable(tab, frn)` 直接接走，而 `Mutable` 要长期握着这张表按 USN 增量改。
`debug.FreeOSMemory()` 只能回收构建期的临时数组，176 万条的**活表**它回收不了
（活对象对 GC 不可见为垃圾）。

但服务每次启动都会重扫一遍全量——`runIndexLoop` 首轮就是 `rebuild`——堆表没有任何
后续用途。于是改成 **落盘 → 丢掉堆表 → `index.Load` 回映射刚刚写出的文件**：

```go
binPath, err := index.Save(...)
total := tab.Len()
tab = nil                        // 300 MB 的活表在这里放手
mapped, _, err := index.Load(binPath)
return mut, base, nil            // mut 是握映射基座（或退回堆表）的那个，见第四轮
```

三件事让这个改动几乎零成本：

- **语义不变**：基座只读、增量全走 overlay（`appendOverlay` / `markDead`），
  和主进程用的是同一套 v4 结构——这正是第三轮改造留下的能力。
- **FRN 不必落盘**：`FRN → 堆表下标` 的映射在 `BuildAll` 里已经建好，而
  **落盘顺序就是内存下标顺序**，直接复用即可。原本估的"必须先升 v5 格式加 FRN 列"
  这个卡点并不存在。
- **物理页可共享**：服务端和主进程映射的是同一个 `index.bin`，Windows 下同一文件的
  映射区共享物理页，那 73.8 MB 不会按进程翻倍。

稳态再补一手：`debug.SetGCPercent(40)`。活堆只有 33 MB（其中 26 MB 是 FRN 映射表），
默认 100% 会让 Go 一直留着 66 MB 的空闲区——实测 `HeapSys 759 MB / HeapReleased 702 MB`，
差的 57 MB 就是"目标堆"余量。构建期仍用 100%，否则那 15 秒里 700 MB 量级的分配会被
GC 反复打断。

| 指标 | 改前 | 改后 |
|---|---|---|
| WorkingSet | 292.8 MB | **102 MB** |
| 私有提交 | 313.7 MB | **108 MB** |
| Go 活堆 | ~250 MB | **33 MB** |
| `index.bin` 映射 | 1.2 MB（等于没映射） | 73.8 MB（按需驻留） |

日志里多一行 `索引常驻: 活堆 33 MB / 向系统要了 759 MB / 已归还 702 MB`：
`HeapReleased=702 MB` 说明构建期那批分配真的还给了操作系统，不是靠 `FreeOSMemory`
把数字做漂亮。

剩下的 108 MB 构成：33 MB 活堆（FRN 映射表 26 MB 是大头）、约 20 MB Go runtime 自身的
提交、其余是索引映射里被搜索 touch 过的页。再往下压只剩一个方向——把 FRN 映射表也落盘
（8 B/条 = 14 MB，代价是从"哈希直查"改成"排序副本 + 二分"，省约 12 MB），
收益对不上复杂度，暂时不做。

### 结果（真机实测，1,766,146 条）

| 指标 | 原始 | 第一轮止血 | 第二轮改格式 | 第三轮 mmap+overlay |
|---|---|---|---|---|
| Private | 526 MB | 369 MB | **269 MB** | **173 MB** |
| WorkingSet | 490 MB | 327 MB | **223 MB** | **151 MB** |
| Go heap | 457 MB | 209 MB | **97 MB** | **8 MB** |
| 堆对象数 | 2,092,272 | 2,092,272 | **48,7xx** | 4x,xxx |
| 索引文件 | 23.47 MB | 23.47 MB | 23.42 MB | 73.8 MB |

对象数降 97.7% 比字节数更关键：GC 每次要扫的活对象少了一个数量级，
`gc=65` 之后曲线完全躺平（`heap=97 inuse=111 sys=264`，几十轮采样不变）。
第三轮之后 `heap=8MB sys=16MB`——索引已经不在堆里了，剩下的 WS 是玻璃的全屏快照、
图标缩略图缓存和 D3D 表面，跟索引规模无关。

### 第四轮：FRN 表落成旁挂文件 + 空闲交回工作集（WS 29 MB）

第三轮把索引本体换成了映射，但 WS 仍在 100 MB 上下，而且 Go 活堆里还压着 33 MB。
`go tool pprof -top -inuse_space`（`BAOZI_IDXHEAP=<路径>` 落快照）一眼就能看出是谁：
`FRNMap.alloc` 30720 kB，占活堆 83%。

那是 `FRN → 下标` 的开放寻址表：177 万条按 0.8 装载率得开 2.6M 槽 × 12 B ≈ 30 MB。
它和索引本体一样是**只读数据**，没有任何理由常驻堆。

改法（`internal/index/frnfile.go` 新增）：

1. 重建末尾把 `(frn, idx)` **按 frn 升序**写成旁挂文件 `index-<代>.frn`
   （头 16 B：`BZFR` / ver=1 / count；记录 12 B：frn u64 + idx u32），177 万条 21 MB。
2. 服务端 `LoadFRNFile` 映射进来，查一次是**二分**（21 步、局部性好，比原来那趟随机探针
   还便宜），堆里只剩"重建之后新增/改名的那几十上百条"。
3. `Mutable` 加 `base *FRNFile`，`NewMutableBase` 用它当基座；`lookupFRN` **增量优先、
   再查基座**（改名会把老 FRN 指向新下标，顺序不能反）；`setFRN` 只写增量。
4. 落盘/映射失败就退回堆表（日志 `FRN 表落盘失败（退回堆表 N 条）`）——宁可多占 30 MB，
   也不能让增量查不到 FRN。

再加一手"空闲就把工作集交回系统"：`idleTrimInterval=60s`，条件满足时调
`SetProcessWorkingSetSize(-1, -1)`（效果等同 `EmptyWorkingSet`）。它**不丢数据**，
只是把干净页从工作集摘回 standby，下次访问是软缺页。之所以要做：这些页是只读文件映射，
进程安静时 Windows 从不主动收，任务管理器就一直挂着一百多兆。

| 指标 | 第三轮 | 第四轮 |
|---|---|---|
| WorkingSet | 102 MB | **28.6 MB**（空闲后） |
| 私有提交 | 108 MB | 68.5 MB |
| Go 活堆 | 33 MB | **3 MB** |
| `.bin` + `.frn` 映射 | 73.8 MB | 73.8 + 20.2 MB（按需驻留） |

**这一轮踩的坑（值得单独记住）**：`rebuild()` 末尾当时还是
`return index.NewMutable(mapped, frn), base, nil`。FRN 成功落盘后 `frn` 被置 nil，
于是返回给 `runIndexLoop` 的 `Mutable` **一个 FRN 也查不到**——所有 USN 记录都在
`Apply` 里被静默丢掉（副作用是进程全程"空闲"，WS 反而一直是被 trim 过的 3 MB 假象）。
症状只有两行：服务日志 `增量: 读到 N 条 USN 记录，但一条都没用上`、面板日志停在
`服务已更新快照` 之后再没有 `已同步 N 条变化`。诊断靠两个小工具：

- `go run ./tools/frnreplay -bin <index-xxx.bin> -frn <index-xxx.frn> -vol C -state <index.progress.json> -data <数据目录> -limit 30`
  用服务端那套结构（映射表 + 映射基座 + 覆盖层）回放一段 USN，逐条打出
  `frn / parent / create / del / dir / reparse / hidden / 应用结果`，
  哪道判断挡住的、父节点到底在不在基座里，一眼就看见（这个工具反而证明 Apply 本身没问题）。
- `go run ./tools/frnprobe -file <index-xxx.frn> -key 0x00020000000b083a`
  直接查某个文件号在旁挂表里能不能命中，并打印表头几条记录的键
  （键的高 16 位是盘符：`volKey` 把"卷内唯一"的 FRN 提升成全局唯一）。

教训：**"进程不干活"和"进程内存低"是两件事**，前者永远先查链路有没有断，再谈优化。

### 踩过的坑

- **py 记录宽度写错**：`Save` 里借 4 字节的 `buf` 写 2 字节长度，实际写了 12 字节，
  而 `pyRecLen=10`，整段错位——读回来的拼音串是一大截乱码（`PyOf` 返回跨几千字节的字符串）。
  症状是拼音搜索全部失效。定长格式必须让"写出去的字节数"与"声明的记录宽度"严格相等。
- **`config.normalize()` 会把空 `roots` 复原成 `SystemRoots()`**，所以拿不到"零索引基线"，
  想量纯 UI 只能改代码或换数据目录。
- `unsafe.Slice` 要求段起点按 4 字节对齐，所以 `nodesOff` 要 `align8`；
  对齐不满足时代码回退到逐条解析，不会崩。
- 新格式读回来之后 `Nodes`/`Names` 都指向那块 `raw`，`Table` 必须一起持有 `raw`，
  否则切片会悬垂——`raw` 字段就是为这个留的。

## 16. 面板玻璃、圆角，以及"右侧下侧拖不动"的根因

### 分层

玻璃分支里三层叠着（`cmd/baozi/main.go` 的 `ui.Column(c).Fill()...`）：

| 层 | 内容 | 圆角 |
| --- | --- | --- |
| 底 | `ui.Column(c).Fill().Background(a.deskColor).DragWindow()` | 不裁 |
| 中 | `ui.Image(c, a.frost)` 铺满（磨砂快照裁块） | 不裁 |
| 上 | `ui.Column(c).Absolute().Left(0).Top(0).Size(pw, ph).DragWindow()` 放 body | 不裁 |

**圆角只由窗口区域一套提供**，三层都不裁。这是修了好几轮才定下来的：
区域铺的是**整个窗口矩形**、内容画在**客户区**，两者圆心天生差 9px（左右各 9px、
下边 9px 的边框带）。只要内容层也裁一次圆角，四角就是两套圆弧交叠出来的
"方角套圆角"，半径怎么调都对不齐。而区域外的像素系统根本不显示，内容画满
客户区没有任何副作用。

`pw, ph := a.panelSize()` 必须是显式尺寸。早先写的是
`.Left(0).Top(0).Right(0).Bottom(0)`，在 `Overlay` 那一层 `Right/Bottom` 撑不开、
高度只按内容算，底栏那一截跑到层外，按下去命中的是最下层——那一层当时没有
`DragWindow`，于是"底栏按下去不动"。改成显式 `Size` 之后层高才是窗口高。

最下层挂 `DragWindow` 是兜底：任何没被上层元素吃掉的点，命中的都是它。

### 圆角：视觉靠 ClipInset，阴影靠 SetWindowRgn

`applyRoundRegion()`（`cmd/baozi/glass.go`）只给**顶层窗口**设圆角区域，
半径 `glassRadius * glassScale()`（DIP → 物理像素）。DWM 的阴影按窗口区域画，
不设的话阴影四角是方的，糊在圆角内容上看着还是直角。

**这里踩了两个坑，都跟"区域就是命中区"有关。**

**坑一：区域尺寸用了客户区，导致右边和下边整条不可点。**

`SetWindowRgn` 的区域坐标是**窗口坐标**（相对窗口左上角，含非客户区），
而最初写的是 `ClientRectPixels(hwnd)` —— 那是客户区。这台机器上面板窗口是
**1098×638**，客户区只有 **1080×629**（左右各 9px、下边 9px 是 DWM 给阴影留的边）。
于是 `CreateRoundRectRgn(0, 0, 1080+1, 629+1, ...)` 从 (0,0) 铺出去：
**右边 18px、下边 9px 落在区域外**。

区域外的鼠标事件不属于这个窗口，会直接穿透过去，UI 层连 pointer down 都收不到。
症状就是用户报的**"从面板右侧、下侧按下去拖不动，上边和左边正常"**——左边和上边
从 (0,0) 起，正好一点都不缺。

用 `GetWindowRgn` + `GetRgnBox` 读出来的包围盒是 `0,0-1080,629`，实锤。

**坑二：改成窗口矩形，命中区补满了，但四个角错位。**

第一版修法是直接把区域换成 `WindowRectPixels`（1098×638）。右边和下边确实能按了，
但圆角位置错了——内容层的 `ClipInset` 是**相对客户区**裁的，区域却以**窗口外沿**为
基准，两个圆心差 9px，四个角各自"多切一块/少切一块"，角外挂着一小块底色。

中间试过"区域铺客户区矩形 + 客户区偏移"（`ox, oy := cx-wx, cy-wy`）：圆角确实和内容
同心了，包围盒 `9,0-874,564` 也对得上。但那**把边框带又切回区域外面**了——右侧和
下侧那圈 9px 正好是缩放热区，跟着一起没了。用户随后澄清"我说的是缩放，我是不是说错
了"，这条路就废掉了：面板的缩放入口只有那圈边框。

**最终做法：区域铺整个窗口矩形，四角月牙交给 deskColor。**

```go
_, _, w, h := WindowRectPixels(hwnd)
CreateRoundRectRgn(0, 0, w+1, h+1, r*2, r*2)
```

区域覆盖到窗口外沿，右、下、左三边那圈 9px 边框带重新落回区域内，缩放热区跟着回来。
代价是客户区四角会露出一片月牙（区域圆角圆心与 `ClipInset` 圆心差 9px，最厚 9px）。
那片月牙露的是最下面那层 `deskColor`，只要它和面板周围的实际桌面同色就看不出来——
所以 `glass.go` 取底色的地方从"窗口左上角外一点"改成了**窗口四周八个点求平均**
（单取一点时经常落在壁纸的高饱和区域，实测取到过 `#00DBEA` 这种亮蓝，月牙跟着发蓝）。

**坑三：给子窗口也设区域，等于把四角挖成死区。**

曾经为了"四角真的透明"，用 `EnumChildWindows` 给每个子窗口套同一个区域
（`RoundChildRegions`）。但原生 UI 画在一个铺满客户区的子窗口里，给它套 20px
的圆角区域后，四个角那一小块按下去毫无反应——**最要命的是右下角，那正是拖面板时
手最容易落下去的地方**。

这个做法已经删掉。四角根本不需要子窗口区域来挖空：顶层区域已经把圆角切出来了，
区域外的像素系统不显示，内容画满客户区也不会露出来。
`RoundChildRegions` 连同它的包级回调和 `syscall` 依赖一起删了——
留着就是一个随时会被人重新调用的雷。

### 第四版：客户区铺满整个窗口，命中测试自己算

前三版都在"区域"和"内容"之间找平衡，但绕不开一个几何事实：面板窗口比客户区大一圈
（左右各 9px、下边 9px），那圈是 Windows 给可缩放窗口留的**不可见**边框。
区域画圆角、内容画矩形，差这 9px 就永远对不齐；区域缩回客户区，缩放热区就跟着没了。
左下、右下两个角尤其明显——客户区没铺到窗口底边，那 9px 没有任何内容可画，
透出来是桌面，看上去就像被啃掉了一块。

真正一次修好的是把**客户区铺满整个窗口**（Electron 的 `frame: false` 就是这么做的）：

```go
// .patched-mygo/internal/windows/window.go, frameCalcSize
windowRect := params.Rgrc[0]
procDefWindowProcW.Call(w.hwnd, wmNCCalcSize, wp, lp)
params.Rgrc[0].Top = windowRect.Top
params.Rgrc[0].Left = windowRect.Left
params.Rgrc[0].Right = windowRect.Right
params.Rgrc[0].Bottom = windowRect.Bottom
```

客户区、窗口矩形、圆角区域从此是同一个矩形，四角只剩一套圆角，半径随便改
（`glassRadius` 默认 24 DIP，设置里可改）。内容层也不再自己裁圆角，只留一圈跟圆弧重合的描边。

**代价是自己接管命中测试。** 客户区铺满之后每个点都在客户区里，`DefWindowProc`
一律回答 `HTCLIENT`，四条边的缩放全丢，按在边上只会拖动窗口。要补两处：

1. `window.go` 的 `resizeHitTest`：屏幕坐标转客户区，距边小于边框厚度返回
   `HTLEFT/HTRIGHT/HTTOP/HTBOTTOM`，距角 16px 以内返回角（和 Chromium 的框一致）。
2. `surface.go` 的 `message`：UI 画在铺满客户区的**子窗口**里，鼠标永远落在子窗口上，
   主窗口的 `WM_NCHITTEST` 根本不会被问。子窗口要自己回答，并且把随之而来的
   `WM_NCLBUTTONDOWN` **转发给顶层窗口**。不转发的话系统把消息发给子窗口，子窗口
   不会缩放，表现就是"光标已经是缩放箭头、但拖了没反应"——`topedge.go` 的顶边
   窗口也是这么转发的。

实测（窗口 1143×665）：右边 → 1203、下边 → 725、左边 → 1203、上边 → 725、
右下角 → 1193×705，全部是缩放，不是移动。

**四角偶尔"看不见圆角"不是几何问题。** 左下、右下露的是磨砂玻璃透出来的桌面色，
玻璃够透的时候它和桌面**颜色接近**，边界自然不明显。

**四角的锯齿怎么消掉的（2026-11-18 定案）。** Win10 的窗口形状只能走
`CreateRoundRectRgn`，它是 1 位掩码、弧线天生 1 像素台阶，**没有抗锯齿**；试过的
两条捷径也都不通（`ClipInset` 的 radius 在 D2D 后端不实现；窗口靠 alpha 透桌面时
DWM 直接填 `#FFFFFF`）。所以弧上那圈台阶只能自己画：

1. **窗口区域**（`applyRoundRegion`）按 `glassRadius` 切，管形状和 DWM 阴影；
2. **四角月牙位图**（`buildCornerBitmaps`）盖在四角，尺寸 `region+10`。掩码由
   4×4 子采样算出（"这个像素有多少面积落在弧外"），再在掩码上做**两遍 3×3 柔化**，
   最后按掩码把桌面色铺上去。实测 `(0,30..33)` 依次是 `#15A4F2` → `#61BEF4` →
   `#5FBEF4` → `#5BBDF3` → 边线，是一条 3 像素的连续过渡。
   区域之外一律不透明（那里是 DWM 填的白）；弧内透明，露出磨砂玻璃。

**弧的过渡带为什么不直接用"到圆弧的距离"算** —— 这是被用户连着驳回两轮的坑：

- 用"到圆弧的有符号距离 ±2 像素做线性渐变"：在弧与四条边**相切的点**上，这个
  距离随坐标几乎不变，过渡被拖成十几个像素长的竖带（实测 `x=1` 列从 `y=25` 一路
  混到 `y=35`，在壁纸上看就是一枚亮刺）。
- 只用量化覆盖率：各方向都只过渡 1 像素，可壁纸深蓝、玻璃浅灰，一个像素跨 165
  级亮度（`#15A4F2` 直接跳到 `#A2D4F5`），那还是一道硬台阶。
- 两个合起来才成立：覆盖率保证"形状正确、各向同性"，**在掩码图像上**做的 3×3
  柔化把 1 像素摊成 3 像素。柔化跟弧的方向无关，所以切点也不会被拉长。

**柔化越界采样必须分方向 —— 这条漏了两次，第二次是用户描出来的那两条框线。**
柔化要在位图边界之外取值，补什么值取决于是哪一侧：

- 贴窗口边的两侧，外面是"窗口之外"，那里是 DWM 填的白，必须补 1（不透明），否则
  弧外漏一圈白；
- 朝面板内部的两侧，外面就是玻璃，必须补 0（透明）。

早期一律补 1。月牙位图是 40×40、弧半径只有 30，朝内那两条边离弧还有 10 像素，
本该整条透明，结果被柔化拉到 0.33 左右，沿整条边铺出一带"桌面色"。因为位图是方
的，这一带在面板里就是**一条水平、一条垂直的边**——外观像一个随壁纸变色的矩形
色块。用户截图里描的那两条红线标的正是它（左边距面板右边 30px、上边距底边 29px，
量下来是位图内边那一侧）。
`cornerOuterSide(corner, size, x, y)` 按角决定哪两侧算"窗口之外"。修复后 1px 分类图
实测：左下角距底边 37~60 行整片回到玻璃色，横带与对应的竖带同时消失。

**弧外铺的不是"一个角上的取色"，而是逐像素照抄屏幕。** 早期每角只存一个采样色，
壁纸在那块只要有渐变，月牙就成了一道亮带（实测弧外 `#79C5F7`、紧挨着的真桌面
`#15A4F2`）—— 这也是用户说"还能看出来"的来源之一。现在 `captureCornerPatches`
在 `panel.Show()` **之前**用 `win.CaptureRect` 抓四角各 40×40 的真实像素（显示之后
那块就是面板自己了），月牙的每个像素用它自己的颜色，弧外与真桌面 1:1 同色。
四张图一共不到 30 KB。

**抓之前必须先问窗口可不可见 —— 这一条漏掉过，代价是四角一整块色斑。**
`refreshGlass` 名义上在 `Show()` 之前跑，但连续唤出、改尺寸触发 resize 这些路径上
窗口本来就是可见的，此刻抓下来的"桌面色"其实是面板自己（或者它没画完时 DWM 填的
`#FFFFFF`）。月牙把它照抄到弧外，四角就出现一块和真桌面不同的矩形色斑；因为那块
是矩形 patch，色斑的内侧会留下一条水平、一条垂直的边 —— 看上去就是"角上凭空多了
两条直线"。所以现在 `captureCornerPatches` 开头先 `win.IsVisible(hwnd)`，可见就
直接返回、沿用上一次隐藏时抓的那份；另外 `patchLooksBlank` 做兜底，整块 70% 以上
像素接近 `#FFFFFF` 时判为无效、同样沿用旧图。日志上能从
`玻璃: 桌面底色 #FDFDFD` / `月牙重造 … 角0=FBFBFB`（抓错）变成
`玻璃: 桌面底色 #417389` / `角0=52ADF2`（真实青蓝桌面）来区分。

**半径定在 24 DIP，弧和区域同半径、同圆心。** 之前取 26 DIP 让弧比区域小 2 DIP，
"小一圈"是为了让区域那圈 1 位掩码的硬边落在月牙铺的桌面色里 —— 但那是"每角一个
采样色"时代的补救，现在的月牙本来就是逐像素真桌面，硬边两侧天然同色，不需要再让。
而让出去的直接后果是四条直边线的起点够不到弧的端点，角上断出一截，看起来就是
"圆角的地方没有边框"。现在 `glassClipRadius == glassRadius`，直边线从
`glassRadius` 起画，正好落在弧的端点上。

**半径一度收到 16 DIP，现在放回 24 DIP，因为 16 那种"直线过长"本身就是问题。**
16 DIP 只跨 20 物理像素：一条直边走将近一千像素、到最后 20px 才拐弯，看上去不是
"圆润的角"而像"直角被削掉一个小角"（用户截图 71×58 那块右下角：竖线直着下来到
`y=24` 才起弯、横线直着过来到 `x=38` 才起弯，中间只有 17~18px 的弧）。26 DIP 那版
当时被否的理由是"弧看着画进了边框内部"，但那是当时的整圆 bug（判定没限定在角象限，
面板内部又画了四分之三圈），已经修掉了，所以半径可以放回来。24 DIP×1.25 = 30px，
实测直边端点距窗口边 28px 处起弯，与 30px 半径吻合。

**弧上那 1 DIP 的边框是画在月牙位图里的，不是四条直边线延长过来的。** 四条直边线
如果延长到弧上，会被窗口区域那条 1 位掩码边界裁断（弧的一侧就在区域外）。所以边框
由月牙自己画：在"弧内侧 0..1 DIP"这一圈上把颜色混到 `edge`、alpha 抬到至少 w。
这一段取**满覆盖**，不做"距弧远近"的衰减 —— 衰减会让弧上那条线比直边淡一档
（实测弧上 `#CDD6DD`、直边 `#D9D9DE`），角上看起来就是"边框到了弧就变浅了"。
只在带子内外各留 1 DIP 柔化，弧上和直边同色同宽。

**四条直边线内侧各补一条 30% 的柔化边，和弧带同宽。** 弧带是"1 DIP 满覆盖 +
内外各 1 DIP 柔化"，视觉宽度约 2 DIP；直边线只是一条 1 DIP 的实心矩形。两者在
弧端点接上时宽度从 1 DIP 突变到 2 DIP，接口处看着就是"一条细直线接到一段较粗的弧"
—— 用户描那个 Γ 时，左边距面板右边正好 30px（= 弧半径）、上边距底边 29px，标的
就是这个交界点。补上柔化之后直边区实测 `#D9D9DE` + `#B8B6BB` 两像素，和弧带一致。

**四条直边线的两端各多画 1 DIP 压进弧段。** 精确相接时，直边线终点按"面板 DIP 宽度"
算出来、弧端点却落在"窗口像素宽度"上，两者差 1~2 像素，接缝处露出一小段缝：
看上去是一条直边线孤零零地断在角旁边（截图实测底边线画到 `x=37`，`x=38-39` 空了两
像素弧才接上）。压进弧内的部分同色看不出重叠，压到弧外的部分由最后绘制的四角月牙
盖住（月牙弧外是真实桌面色，alpha 不透明）。部署后实测：窗口右下角最底行
`x=W-45..W-20` 连续 `#D9D9DE`，末端正好落在弧端点，无缺口。

实测（`318,561 1111×628`、scale 1.25、24 DIP 半径）：左上角 `x=0` 列 `y=19` 起是
`#D9D9DE`，`y=0` 行 `x=19` 起同色；右下角底边线最后一个边线像素距右边 28px、
右边线最后一个距底边 28px（30px 半径）；对角线满边线色；角外无白块。

**所有和"弧"有关的判定都只在角那一个象限里成立 —— 这一点漏掉过。** 代码里量的是
"到圆心的距离"，而它对**整圆**都成立：圆心在 `(arc, arc)`、半径 `arc` 的圆在面板
内部还有 3/4，`(2*arc, arc)` 就是圆上一点，那些像素同样满足 `r > arc`，于是被当成
"弧外"铺了桌面色、还被描上边线。屏上的表现就是**弧跟边框交完界之后又往里画了一段**，
一眼就能看出那道弧的形状。所以弧外判定、区域外不透明、弧上边线这三处都加了
`fx <= arc && fy <= arc`；象限外的像素一律是玻璃。

**这里踩过一个坑，也是"改了十几轮、屏幕上没变化"的真正原因**：`ui.Bitmap.Update`
只改 CPU 侧那份像素（`img.Pix = s.Pix` + `Changed()`），**不会重新上传 GPU 纹理**，
屏上一直挂着进程启动后第一次 `NewBitmap` 造的那张图。早期版本在那张图上沿弧画过
一条 `#D9D9DE` 的边线，于是无论后面怎么删边线、改半径，用户看到的都是同一道
白色圆弧。现在 `buildCornerBitmaps` **每轮都 `ui.NewBitmap` 造新对象**（43×43 一张、
四张不到 8 KB，GPU 上传后像素还会被释放），并且 `refreshCorners` 末尾立刻
`panel.Update(func(){})` 重绘一次 —— 不重绘就只是把新图挂上去，屏上仍旧是上一帧。

实测（面板 993×658、scale 1.25、连续三次真实双击 Ctrl 唤出）：四角 45×45 内
纯白 **0** 点、亮度 >238 的 **0** 点；`(1,1)` 是桌面色、`(10,10)`/`(14,14)` 是
`#C1E3FA`/`#C0E3FA` 的玻璃，弧上 `(17,4)`、`(21,2)` 是中间色，过渡连续。
把面板挪到背景色不同的地方再唤出，四角会跟着换色（旧图是换不了的），
说明新图确实上了屏。

### 实测方法（不靠看图）

**一定要用应用自己的唤出路子。** 用 `ShowWindow(h, 5)` 强制显示不触发
`refreshGlass`/`refreshCorners`，测到的是上一帧的残留甚至 DWM 白板 ——
早期很多"代码和像素对不上"的假象都是这么来的。正确做法是 `keybd_event` 双击
Ctrl（`baozi-hook` 认这个），或者手动按。注意双击两次会切到隐藏，
所以**一轮只唤出一次、隔 1.2 秒再按下一次**。

用 PowerShell + `Add-Type` 拼一个探针（全部 P/Invoke 塞进**同一个类**，
因为 `Add-Type` 定义的类型只在单次调用内有效）：

1. `EnumWindows` 按 PID 找标题是 `Baozi` **且 `IsWindowVisible`** 的窗口
   （同一进程里还有「Baozi 设置」「Baozi 文件互传」「MyGoApp」三个窗口）；
2. 采样点：`GetDC(0)` + `GetPixel`，坐标一律用 `GetWindowRect` 的当时结果
   （窗口位置尺寸每次唤出都可能变，别复用上一轮的矩形）；
3. 判白：扫方形 0..44 统计 `==0x00FFFFFF` 与亮度 >238；判锯齿：沿弧按角度扫、
   统计相邻亮度突变 >18；
4. 拖拽测试：`SetCursorPos` + `mouse_event(LEFTDOWN 0x2)` + 多步移动每步
   Sleep 110–180ms + `LEFTUP 0x4`；Sleep 80ms 会误报"无反应"；之后
   `SetWindowPos` 复位（`SWP_NOSIZE|SWP_NOZORDER`）。

修好之后实测（面板 878×510 DIP，窗口 `1098×638`，区域包围盒 `0,0-1098,638`）：

| 抓取点 | 结果 |
| --- | --- |
| 右边框带（客户区右外 4px） | 缩放：`1098×638 → 1158×638` |
| 下边框带（客户区下外 4px） | 缩放：`1098×638 → 1098×688` |
| 左边框带（客户区左外 5px） | 缩放（Left 减小、宽度增加） |
| 右下角尖 3px 内 | 无反应——那是半径 24 DIP 的圆弧本身挖掉的几何缺口 |
| 客户区右边缘内 6px | 无反应——那里已经是 UI 内容，命中的是卡片 |

判"缩放还是移动"只看 `GetWindowRect`：`Left/Top` 变是移动，只有 `Right/Bottom` 变是缩放
（左边缘缩放会同时改 Left 和宽度，所以不能只看 Left）。


## 17. 缩放（拖边改尺寸）为什么会卡，以及三处代价

用户报的现象是"缩放时卡卡的"。逐层量下来，一帧的代价分成三块，只有第一块是我们的：

1. **`OnResize` 里重建四角月牙**（原来是每帧都做）。每一帧四角各跑一遍
   `40×40` 掩码（约 1600 次 `hypot` × 4 角）＋两遍 3×3 柔化＋合成四张图，
   再用 `ui.NewBitmap` 上传四张 GPU 纹理。圆角半径从 16 DIP 提到 24 DIP 之后
   掩码边长从 30 涨到 40，这一项的绝对量又大了近一倍 —— 这就是"现在才觉得卡"。
   修法：掩码与柔化按 `cornerMaskKey{arc, region, size}` 缓存，尺寸不变时直接复用。
   月牙的边长只由圆角半径决定、和面板尺寸无关，缩放期间完全没必要重造。
2. **一帧画两遍**。mygo 的 `surface` 收到 `WM_SIZE` 就自己 `s.drawNow()`
   （`surface.go`：*"Draw at once, so the content follows the window while it is
   resized"*），玻璃的取景偏移也是在那一趟布局里按新客户区重算的。`OnResize`
   里再调一次 `panel.Update` 等于又走一遍布局与合成。删掉之后少整整一层。
3. **每一帧重设窗口区域**。`SetWindowRgn` 改的是 DWM 算阴影、重分配重定向表面
   时用的形状，尺寸一变就要重算一次；而且区域按窗口坐标记，窗口变大时旧区域
   还会把新长出来的部分裁掉。现在一进缩放就 `ClearRegion`（`SetWindowRgn(h, 0)`），
   整段拖动期间窗口是矩形、形状靠四角月牙铺的桌面色撑着，停手 160ms 后
   `finishResize` 再一次性 `applyRoundRegion() + refreshCorners()`（顺序不能反：
   先有区域 DWM 才按圆角裁阴影，月牙再盖上去补抗锯齿）。

实测（外部探针从另一个进程 `SetWindowPos`，它会把 `WM_SIZE` **同步**投递到 UI 线程，
所以耗时包含 app 侧处理）：一帧 5~6ms、最大 9~11ms；`OnResize` 回调本身几乎每帧
都 <1ms（探针阈值 1ms 时 25 帧只有一帧记到 1.02ms）。也就是说剩下的 4~5ms 在
mygo 的 `drawNow`（全量布局 + GPU 合成 + present）与 DWM 同步里，我们能改的只有上面三块。

验收也一起做了：拖动中 `GetWindowRgn` 返回 0（无区域＝矩形），停手后返回 3
（复杂区域＝圆角）；复位后左下角落弧外是桌面色、弧上有 1px 边框、弧内是玻璃，
没有横向或纵向的色带。

## 18. 阴影：为什么只能自己画，以及四角那道白线到底是什么

### 18.1 Win10 上"有区域的窗口没有 DWM 阴影"（实测结论，别再试了）

圆角必须靠 `SetWindowRgn`（Win10 没有别的形状手段，`DWMWA_WINDOW_CORNER_PREFERENCE`
是 Win11 才认的），而**窗口一旦有区域，DWM 就不投阴影**。证据链：

- 空闲（区域在）时，窗口外侧整条带取色全是 `#FFFFFF`；
- 按住拖动的那一瞬间（mygo 会临时撤区域）才冒出一圈 `+0:#C7C7C7 → +18:#FDFDFD`；
- 松手把区域设回去，影立刻消失。

逐项试过、全部"返回成功但无影"：`DWMWA_NCRENDERING_POLICY`（设 ENABLED 返回 S_OK、
属性回读也是 ENABLED，影照旧没有）、`DwmExtendFrameIntoClientArea(1px)`、两者的顺序组合、
外部进程里同样组合。另外 mygo 的 `SetHasShadow` 开头就是
`if !w.captionless() || w.vibrancy != "" { return }` —— 只要 `WindowOptions` 里带
`Vibrancy`，连 `DwmExtendFrameIntoClientArea` 都不会走，所以面板现在**不设 Vibrancy**
（Win10 上它本来也没有视觉作用，只是把材质路径换成 `win.SystemGlass()`）。

### 18.2 做法：一个自绘的 layered 窗口

`internal/win/shadow.go`：`WS_POPUP | WS_EX_LAYERED | WS_EX_NOACTIVATE | WS_EX_TOOLWINDOW`
（**不带 `WS_EX_TRANSPARENT`**），窗口类 `BaoziShadowWnd` 自己注册，位图用
`UpdateLayeredWindow` 交 32 位预乘 BGRA。四道坑：

1. `WS_EX_TRANSPARENT` 会让这个分层窗口**整片不参与合成**（不是"鼠标穿透"那么轻）。
   穿鼠标改成在 `WM_NCHITTEST` 里回 `HTTRANSPARENT`。
2. SDF 必须用标准式
   `hypot(max(qx,0), max(qy,0)) + min(max(qx,qy),0) - r`。当初写成 `hypot(qx,qy)-r`
   直接用原始值，只要有一轴深入面板内部（`qy` 是很小的负数）就被撑爆，整条直边
   alpha 全 0 —— 屏幕上就是"只有四角有影、四条边光秃秃"。
3. 阴影层**不能垫在面板下面**：垫着实测完全看不见（位图 alpha 明明有值、
   `UpdateLayeredWindow` 也返回成功）。改成 `HWND_TOPMOST` 压在面板上面，
   面板内部 alpha 恒 0，所以不影响玻璃。
4. `offY` 不能偏移。曾把图层整体下移 `m/8` 假装光源在上，结果底边那几行 alpha
   没归零，阴影在图层最下一行被硬切，屏幕上是一道水平直角硬边。

### 18.3 四角那道白线：只能由这一层补

面板被 `SetWindowRgn` 裁形之后，**弧外那块是 DWM 填的白，面板自己的像素在区域外
直接被丢弃** —— App 里的"月牙"位图根本盖不到那块白，能画出去的只有弧里侧半截，
3×3 柔化又把它摊成压在玻璃上的浅色弧线，正是用户反复说的"四角内部白圆弧 / 漏光"。
现在四角全部交给阴影层（`UseCornerBitmaps = false`，月牙代码保留但不再摆放）：

- `cornerCoverage(radius, margin, borderW)` 在**角部局部坐标**下做 4×4 超采样，产出三份表：
  `cov`（弧外覆盖率）、`shade`（该压多黑，曲线和外圈一致）、`band`（弧内侧 `borderW`
  像素的边框覆盖率）；按 `[radius, margin, borderW]` 缓存，与面板尺寸无关，缩放一帧不用重算；
- 四个角按 `patchSize-1-lx|ly` 镜像复用，只补"面板矩形以内"的缺口；矩形以外保持纯黑阴影
  （否则拖动时旧 patch 会在影子里印出一块过期壁纸）；
- 弧外像素 `color = patch*(1-shade)`、**alpha = 255**（涂实：那块本来就在轮廓外，
  半透明会把边界那一格的白漏出来）；
- 弧内侧紧贴边界的 1px 涂成不透明边框色（`seam`），再往里 `borderW` 像素按覆盖率做抗锯齿
  —— 这样弧上的边框和四条直边连续、颜色一致。

> 顺带一个观察：面板自己在边界上有 1px `#FFFFFF`（mygo 的"白底 + 裁形"留下的），
> 直边上也有、并且和背景无关。所以现在弧和直边一样，是一条连续的边框线，
> 不会再出现"只在弧上有、直边没有"的那种白弧。

### 18.4 缩放跟手：从 15~25ms 打回 6ms 以内

阴影层每帧重建的代价一开始是灾难性的：`Place` 走遍整张 `1143×764 ≈ 87 万`像素，
一帧 15~25ms，肉眼就是卡（用户原话"太不跟手了"）。三层优化：

1. **只画边带**：面板盖住的内部 alpha 恒 0，没必要算 —— 降到"四条 m 宽边带 + 四个角块"。
2. **直边一行/一列共用一个值**：顶/底带里只要这一列不在四角范围（`qx<=0`），`d` 就等于
   `qy-r`；左右带同理。真正要算距离的只剩四个角块。
3. **`math.Hypot` → `math.Sqrt`**：内层循环里 Hypot 为了防溢出做的缩放很贵（几千像素差 2~3ms）。

实测（`阴影: 重建`/`缩放: 回调` 两条日志）：位图 **1.0~1.5ms**、
`UpdateLayeredWindow` **1.0~2.0ms**，一次 `Place` 合计 3.0~3.6ms。

**缩放期间仍然把阴影层收起来**（`hideShadow()`，停手 160ms 由 `finishResize` 重建）。
两条原因，都是实测撞出来的：

- 每帧重建虽然只有 3~4ms，但它叠在缩放回调原来的 5~6ms 上，Performanced 上就是
  "回调 8~10ms"，而用户对跟手最敏感；
- 面板自己的图层在那一帧还没按新尺寸重排，四角补的桌面色/边框弧会错位露在玻璃外，
  屏幕上就是"缩放时能看到四角画了道弧"。

> 阴影"常驻"指的是空闲状态（唤出后一直有影），不是拖动的那 0.x 秒。
> 顺带记一个真·卡死：`UpdateLayeredWindow` 拿的屏幕 DC 早期从来没有 `ReleaseDC`，
> 缩放每帧漏一个，几十帧就把进程 DC 池吃光 —— 表现是整段拖动卡死，
> 现在 `GetDC`/`ReleaseDC` 成对出现（`GDI 对象数` 160 帧缩放 64 → 65，不再增长）。

### 18.4.1 四角桌面色必须是"当前位置"的

四角弧外铺的桌面色早期是 `BitBlt` 现场抓的 patch（只在唤出前抓得到，
因为面板一可见抓到的就是面板自己）。面板被拖动或缩放之后那块 patch 就过期了，
壁纸是照片时颜色明显串味 —— 用户看到的就是"角上画了一道弧"。

现行做法：唤出时另存一张**半分辨率整屏快照**（`deskHalf`，`2560×1440 → 1280×720`，
3.7 MB，面板收起就释放），`cornerPatchesAt()` 每次按**当前窗口矩形**从这张图里
双线性裁四块 `radius+10` 见方的角块。这样无论面板被拖到哪、缩成多大，
弧外颜色永远和真实桌面一致，而且不用为了抓图把阴影层藏一下。

### 18.5 复验工具

- `tools\shot.ps1`：面板已可见就不按 Ctrl，只把"外扩 40px"的窗口图存成 `dist\shot_panel.png`；
- `tools\resize.ps1`：从外部进程 `SetWindowPos` 连续放大/缩小各 16 帧，回读 `缩放:` 日志；
- `tools\resize_shot.ps1`：子进程跑帧序列、父进程半拍后截屏，用来确认**缩放过程中**
  四角仍然是圆的、阴影还在；
- `tools\shadowprobe`：不依赖 app，单独把阴影层摆到指定矩形并 dump alpha 图（`dist\shadow_alpha.png`）。

> 注意：`GetPixel(GetDC(0))` 之类的屏幕抓取**拍不到分层窗口**，验证渲染只能截图。
### 18.5.1 "快速拖动会卡死"：合成拖动复现不出来，但真机上的重活在移动那条路上

`tools\drag_probe.ps1` 用 `SetCursorPos` + `mouse_event` 合成拖动，同时在每一步
`SendMessageTimeout(WM_NULL, 40ms)` 探 UI 线程是否卡住。实测（面板 1838×778）：

- 移动 600 步、每步之间不睡（≈1300 次/秒）：卡死 0 次，松手后 2.5 秒内也 0 次；
- 右边缘缩放 400 步（≈750 次/秒）：`缩放: 本轮 41 帧，跨度 447ms，回调 avg 0.50ms
  max 1.01ms` —— 我们的回调根本不是瓶颈。

真正的重活在**移动**（不是缩放）那条路：`OnMove` 原来每来一次 `WM_MOVE` 就
`panel.Update()`，那是"整棵内容树重新布局 + 重新栅格化 + 上传纹理"，1100×724 一次
几毫秒到十几毫秒。而拖面板走的是系统模态拖动循环（mygo 的 `DragWindow` =
`SendMessage(WM_NCLBUTTONDOWN, HTCAPTION)`），每秒几百次位移就是几百个重建排进主线程
队列 —— 手感是"拖着不动"，松手后再用几秒钟把队列啃完，观感就是卡死。

现在：

- `OnMove` 里按 **50ms** 节流，并且**一次只排一个**（`moveUpdateQueued` 闸门）：
  队列里已经有一个就不再加。松手 120ms 后补一帧，把磨砂图的取景位置对齐；
- `finishResize`（自治区圆角区域 + 重造四角位图 + 重建阴影）改走
  `mygo.RunOnMain`：`buildCornerBitmaps` 要造 `ui.Bitmap`（GPU 纹理），而
  `time.AfterFunc` 的回调跑在普通 goroutine 上，和渲染线程同时动 D3D 设备没保证；
- 加了**看门狗**（`startWatchdog`）：拖动期间每 200ms 检查一次心跳
  （`OnMove`/`OnResize` 更新 `uiTick`），超过 1.2 秒没心跳就把所有 goroutine 的栈
  打进日志（`看门狗: 拖动中 UI 线程 NNNms 没有心跳`）。合成拖动抓不到的死锁，
  真机上再发生时日志会直接说是卡在哪一行。

### 18.5.2 缩放停手那一下的卡顿：四角取色（GetPixel）比什么都贵

`finishResize` 里那三步加了分段计时（`缩放: 收尾 %.2fms（区域 + 四角 + 阴影）`），
实测（面板 1100×724，松手后）：

```
缩放: 收尾 197.53ms（区域 0.51 + 四角 121.26 + 阴影 2.42）
缩放: 收尾  67.12ms（区域 0.00 + 四角  62.43 + 阴影 4.65）
```

四角那一步（`pickDeskColor` + `refineCornerColors`）要读 8 + 12 = 20 个屏幕像素，
而 `ScreenPixel` 每个点都走一趟 `GetDC(0)` / `GetPixel` / `ReleaseDC` ——
在 DWM 合成下每次 `GetDC` 都要等桌面读回来，**20 个点 60~120ms**，
正好砸在"缩放松手"那一帧上。

两手都改了：

1. `win.ScreenPixels`：一次 `GetDC` 读完一串点（不再每点一趟 DC）；
2. 更要紧的是**根本不去读屏幕**：唤出时那张半分辨率整屏快照（`deskHalf`）
   本来就在内存里，`colorAt()` 优先从它取样，查不到（贴屏边/快照缺失）才回落到
   屏幕取色；
3. 四角月牙位图现在整个停用了（`UseCornerBitmaps=false`，四角交给阴影层），
   `refreshCorners` 在停用状态下**直接返回**：不取色、不造图、也不排那次 40ms 的
   `panel.Update`。`deskColor`（面板四角的兜底底色）改由 `show()` 里单独取一次。

结果：`缩放: 收尾` 不再出现（阈值 10ms 都没到），合成拖动 400 步（≈900 次/秒）
松手后 1.5 秒内 0 次卡死；缩放回调 avg 0.31~0.33ms、max 1.00ms。

顺带一条：`RoundRegion` 里 `SetWindowRgn` 的 bRedraw 传 0。这个区域只决定
"DWM 阴影形状 + 鼠标命中"，不参与内容绘制（内容在交换链里，区域裁不掉）；
传 1 每次调用都让整个窗口失效重画，1100×724 一次 40ms 上下。

### 18.6 角的"毛边"：覆盖率算了却只当布尔用

四角最后定成"2 像素软边"，改动在 `internal\win\shadow.go` 的 `cornerCoverage` 与 `Place`。

原来的三个毛病叠在一起：

1. **`cov` 算了覆盖面积，写像素时却只当布尔**：只要一个像素有一丝落在弧外
   （`cc > 0`），就整块铺桌面色、`alpha=255`。等于边界是 1 位掩码，弧上必然是
   一串 1 像素台阶 —— 这就是"角的毛边"。
2. **`seam` 宽 1.5 像素又涂实**：面板自己那条 1px 边线就在紧邻的那一列里，
   涂到 1.5 会把它盖成第二列、还比玻璃亮，内侧又撞出一个台阶。
3. **窗口区域本身是 1 位掩码**（`CreateRoundRectRgn`），GDI 的整数区域和理想弧
   最多差 1 像素；区域多切掉的那一格只能靠我们涂不透明色去盖，盖的边界就是方的。

改法：

- `cov` 变成"以弧为中线的线性软边"：`cov = clamp(0.5 - d/2.0, 0, 1)`（d 是到弧的
  有符号距离，正数在弧内），宽 2 物理像素。相邻像素系数连续变化，弧就不再是台阶。
- 写像素时按 `cov` 把两侧颜色**混起来**（弧外那侧是被阴影压暗的桌面色，弧内那侧
  是边框色），alpha 仍是 255（这块在面板轮廓外，半透明会把边界那格的白漏出来）。
- `seamW` 1.5 → 1.0，只盖紧邻的那一列。
- 超采样 4×4 → 8×8（band/seam 用的面积覆盖率）。

实测（深色壁纸上的左上角，逐行取水平剖面，弧上一共 2 个过渡像素，相邻行连续）：

```
 y= 8   10  10   9   9   9   8   8   7  69 145 159 ...
 y=10   10   9   9   8   8   7  55 138 158 158 ...
 y=12    9   9   8   8  15 100 158 158 158 158 ...
 y=14    9   8   8  38 131 157 157 157 157 157 ...
```

代价（明确记下，别当 bug）：弧上那条 1px 边框线被这层软边吃掉了 —— 1px 的线
无论怎么算都是二值的，留着它就是留锯齿。四条直边上的边框线是面板自己画的
（区域没裁到），所以直边到弧起点会有一个"线突然消失"的接点。要既要有弧上的
边框、又要没有台阶，只有一条路：**把 `SetWindowRgn` 去掉，整个轮廓（含直边
边框）都由这一层画**，命中区也变回矩形（右下角反而不再有 30px 的死区）。


## 外观状态自愈（ensureShell / panelUp / ForceShow）

四角这套东西的"画"早就定下来了，真正难的是**它偶尔会丢**：用户报的现象是
"面板偶尔变成四个半透明的直角，再按一次热键就又好了"。四个直角 + 半透明
这件事，说白了就是三层东西同时不在：

1. `SetWindowRgn` 设的圆角区域没了 → 面板按矩形出图，四角是方的；
2. 自绘阴影层没上屏（或者没被合成）→ 四角弧外那块"糊上去的桌面色"也没了，
   方角里透出来的就是玻璃本身的半透明色，所以看起来"半透明"；
3. "再激活一次就好"说明它能自愈 —— 只是自愈必须由**一次完整的 show()** 触发。

所以这一轮的思路不是再去改画法，而是**加一层看门狗，让它在 1 秒内把状态补回来**：

- `application.panelUp`（`atomic.Bool`）：我们的"面板此刻应该开着"这个事实，
  `show()` 里置 true、`hidePanel()` 里置 false。**不能用 `panel.IsVisible()`**：
  面板是 mygo 的 noRedirect 合成窗口，系统的 `IsWindowVisible` 在它身上经常报
  false（窗口明明在屏上）—— 改动之前看门狗的门就是这个，结果它一次都没跑过，
  那正是这个毛病能活下来的直接原因。
- 看门狗每 5 拍（200ms×5 ≈ 1s，且 `!dragging()`）在 UI 线程上调一次
  `ensureShell(reason)`：
  - `win.HasRegion(hwnd)` 查不到区域 → 立刻 `applyRoundRegion()` 重设；
  - `shadow.ForceShow()`：**无条件**推一次 `ShowWindow(SW_SHOWNOACTIVATE)`，
    返回值是"推之前是否已可见"，false 就打日志 + `refreshShadow()` 重建位图。
    这里同样不能先问"可见吗"再决定推不推 —— 那个查询会撒谎，等于门永远打不开。
- `show()` 的顺序也改了：先设区域，再 `panel.Show()`，之后再设一次，并且
  `panelSettle+100ms` 后调一次 `ensureShell("唤出动画结束后")`，专门兜
  "mygo 在显示/动画期间把区域弄丢"这种情况。
- `Shadow.Show()/Hide()/Shown()` 都改成按**真实可见性**动作（缓存标志只当加速
  条件）：`Show()` 在窗口其实被藏起来时会真的再 `ShowWindow` 一次，
  `Hide()` 在"标记说藏了、窗口还亮着"时也会真的藏 —— 后者对应另一种老毛病：
  面板藏了、桌面上还留一块孤零零的影。

每次唤出后的前三次复查各记一行，日志里可以直接看到状态：

```
外观复查 唤出动画结束后: 窗口区域=true 阴影上屏=true
外观复查 空闲复查: 窗口区域=true 阴影上屏=true
玻璃: 圆角区域丢了（空闲复查），补设一次
阴影: 层被藏过（空闲复查），强制补回
```

验证办法（`dist\` 下的诊断脚本，都按窗口标题 "Baozi" 找窗口，
**不用 IsWindowVisible**）：

```
pwsh -NoProfile -File dist\_killregion.ps1     # 外部撤掉面板区域
# 1s 内日志应出现"圆角区域丢了…补设一次"，四角回到圆
pwsh -NoProfile -File dist\_hideclass.ps1 -Class BaoziShadowWnd   # 外部藏掉阴影层
# 1s 内日志应出现"层被藏过…强制补回"
```

顺带修掉一个坑：`tools\shot.ps1` 原来按"标题 + `IsWindowVisible`"找面板，
在这个合成窗口上永远找不到，于是退回"双击 Ctrl 唤出"，反而把已经开着的面板
切走了（截图拿到的是空桌面）。现在只按标题找。


## 拖动/缩放时的四角：从"硬边台阶"到"面板自己画弧"

上面那套画法有个前提：**静止时四角是自绘阴影层补出来的**。而拖动/缩放的那 0.x 秒
阴影层是收起的（见 OnResize 的注释），四角这时只剩 `SetWindowRgn` 那道区域边 —— 
GDI 区域的弧按扫描线量整格、没有抗锯齿，屏幕上是"一格一格的台阶"；更要紧的是，
右边和下边正是跟着拖动手柄在变的那两条边，区域和内容只要差半帧，那两个角就**没有弧**
（看起来就是方的），左边和上边不动，所以从来不显。用户报的"拖动时右上、右下有时
变成方角，左上、左下不会"就是这个。

两处改动（都不碰静止时的观感）：

1. `internal/win/region.go` 的 `RoundRegion`：区域半径按 `radius + 2` 建。
   硬台阶原来正好切在弧的抗锯齿带上（那里 alpha 还接近满值），所以不管里面画多细，
   角上都是栅栏。放大 2px 之后，硬边落到弧**外面**那片（面板最底一层铺满窗口、
   本来就是桌面色，看不见），弧该有的软边一个像素都不丢。
2. 拖动期间由面板自己画四张月牙（`main.go` 里 `UseCornerBitmaps || a.dragging()`）：
   弧的几何按**当前窗口像素矩形**算（`win.WindowRectPixels`），所以跟着手柄走，
   不会落后半帧。图只在 `show()` 时造一遍（`refreshCorners(force=true)`），
   因为造完要 `panel.Update` 整屏重绘（40ms 上下），放在"缩放松手"那一帧上就是
   停手时那一下卡顿 —— 月牙只由半径、四角桌面色和边线色决定，和面板多大无关，
   一次唤出造一张足够用到下次唤出。

实测（`dist\_dragcap.ps1` 按住右边缘、每步 14px 边缩边抓帧）：缩放过程的帧里
右上、右下都是连续软弧，没有台阶、没有直角；放大回调 avg 0.34ms / max 1.03ms，
无卡死。另外 `dist\_ab.ps1`（同一进程内做完"有区域/无区域"两次抓图）可以证明
"四角形状**只**由区域给出、内容本身是方的"—— 这也是为什么不撤区域就得靠这两处
改动来补。

### 拖动/缩放时四角为什么会"变方"（以及现在怎么处理）

现象：拖右边或下边改面板大小时，右上、右下两个角有时会变成直角；拖动一次
（整窗移动）不会，左上、左下也不会。

原因很具体 —— **面板的形状只有窗口区域这一个来源**：

- 面板自己的内容（磨砂图 + 底栏 + 图标）是一块**方**的位图，四角没有被它自己圆过。
  实测把窗口区域撤掉（`dist\_killregion.ps1`）再抓角，四个角整块都是玻璃色 ——
  内容根本不知道自己该圆。
- 屏幕上看到的圆角，是 `SetWindowRgn` 那条 1 位掩码裁出来的（GDI 区域的弧是
  按扫描线量的整数格，没有抗锯齿），静止时再叠上自绘阴影层那圈"月牙"补细节。
- 缩放时阴影层是收起的（每帧重建就是卡顿的根源），于是四角只剩区域那道硬边；
  而右边、下边正是跟着手柄在变的两条边 —— 区域和那一帧的内容一旦差半步，
  这两个角就短暂地**完全没有**圆弧，看起来就是直角。左上、左下不跟着动，
  所以"从来不坏"。

两条改动：

1. `internal\win\region.go`：区域半径比画出来的弧**大 2px**。硬边的台阶就落到弧
   外面那片桌面色里（看不见），弧自己的抗锯齿一个像素都不丢。这一步对静止、拖动
   都生效。
2. `cmd\baozi\main.go` + `glass.go`：拖动期间让面板自己把四张角位图（月牙）画
   出来 —— 静止时仍然交给阴影层（它画的弧最干净，两套一起画会叠出白边）。
   位图只在**唤出**时造一次（造完那次整屏重绘 40ms 不能落在"缩放松手"那一帧上），
   月牙只由半径、四角桌面色和边线色决定，和面板多大无关，所以缩放期间一直够用。

实测（拖右边缘缩小，过程中抓帧，10× 放大看右上角）：弧是软的，没有台阶，
也没有直角；日志里 `缩放: 本轮 N 帧 … 回调 avg 0.34ms max 1.03ms`，跟手没退化。

## 关于页与更新检查

设置页最后一项「关于」只做三件事：报当前版本、给 GitHub 地址、说更新状态。

**版本号从 tag 来，不在源码里养常量。** `internal/version` 只有一个 `Version`
变量，发布工作流用 `-ldflags "-X baozi/internal/version.Version=$tag"` 注入；
本机随手 `go build` 出来的是 `dev`，关于页显示"开发版"并**不参与更新比较**
（自己编的版本跟线上比没有意义，只会天天提示有新版本）。

**检查本身不改动磁盘。** `internal/update` 发一次
`GET /repos/l0rraine/baozi/releases/latest`（带 UA —— GitHub 对没有 UA 的请求直接
403），取 `tag_name` 与 windows 那个 zip 的直链；`Newer` 按三段数字比较，不是
字符串比较（`v0.9.0` vs `v0.10.0` 就是会栽跟头的那种）。写磁盘是另一个动作：
动用户的文件必须有人点一下按钮，所以关于页只在发现新版本时给「下载并更新」
（release 里没有 zip 时退回「打开下载页」）。

**为什么它一定不拖慢启动。** 三道闸：

1. `ready()` 里只 `startUpdateCheck()` 一下就往下走，网络全在 goroutine 里；
2. goroutine 自己先睡 3 秒 —— 这时候正在抓屏、扫索引、拉起钩子，抢网络没有好处；
3. 整个请求挂在 `context.WithTimeout(8s)` 上，失败就失败，不重试、不轮询。

关掉的方式：配置里 `updates.autoCheck=false`（关于页那个勾选框），或调试时
`BAOZI_NOUPDATE=1`（不落盘）。`BAOZI_HEADLESS=1` 下也不查 —— 冒烟测试不必去
打 GitHub 的限额。

结果只存在内存里（`application.upd`，一把小锁 + 值快照），界面每帧读一份快照
来画，所以打开设置、滚动、点「立即检查」都不会被网络挡住。有新版本时面板底栏
会顺嘴提一句（"有新版本 v0.4.0 · 设置 → 关于"），不新增控件、不弹窗、不抢焦点。

### 代理

Go 的 `http.ProxyFromEnvironment` 只看环境变量，而 Clash / v2rayN 这类工具改的是
`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`。只按环境变量
取代理，就会出现"明明开着代理，检查更新还是连不上"。所以 `internal/update` 自己
写了一个 `ProxyFunc`，顺序是：**环境变量 → 系统代理（ProxyEnable/ProxyServer/
ProxyOverride）→ 直连**。检查和下载共用 `sharedClient`，两段路的代理行为一致。

只认 registry 里的静态代理，**不解析 PAC**（`ProxyAutoConfigURL`）：PAC 要走
WinHTTP 的 autoproxy 解析器，对一个人一台机器的场景收益太低；公司强制 PAC 的
情况把 `HTTPS_PROXY` 设上就行。`ProxyOverride` 支持 `*.x` / `x.*` / `<local>` /
精确主机名四种写法。关于页的「网络」行显示 `update.ProxySummary()`（直连 /
系统代理 / 环境变量），排查"检查失败"时先看这一行。

### 替换安装目录

Windows 不允许改正在运行的可执行文件，而我们要换的就是自己。所以"下载并更新"
分成两段：主进程只管下载、解压、写脚本、退出；覆盖由一个隐藏窗口的
`powershell.exe` 完成（`cmd/baozi/selfupdate.go`）：

1. 等主进程退出（最多 15 秒，等不到就直接结束）；
2. 只结束**装在同一目录**的 `baozi.exe` / `baozi-hook.exe` / `baozi-index.exe`
   —— 按进程名一律打死会把用户另开的调试拷贝也关掉；`baozi-index` 若是服务，
   先试 `Stop-Service`（没管理员权限就只记日志跳过）；
3. 三个 exe 各自备份成 `.old` 再覆盖，缺哪个记哪个；
4. `Start-Process` 重新拉起 `baozi.exe`；
5. 全部成功才删临时目录，有失败项就留着给人排查。

每一步都写 `%LOCALAPPDATA%\Baozi\update.log`，关于页的「上一次」行读它。

两个踩过的坑：

- **脚本必须带 UTF-8 BOM**。Windows PowerShell 5.1 读没有 BOM 的 `.ps1` 时按
  系统 ANSI（这里就是 GBK）解码，脚本里全是中文，引号会被中文字节的第二个字节
  吃掉，整个脚本变成语法错误 —— 表现就是"点了按钮，然后什么都没发生"。
  沙盒里干跑时就是这么炸的。
- **日志不要用 `Add-Content -Encoding UTF8`**：5.1 每次追加都会再写一个 BOM，
  日志中段出现乱码字节，主程序按 UTF-8 读回去会读到坏行。改用
  `[System.IO.File]::AppendAllText` + 不带 BOM 的 `UTF8Encoding`。
- **启动脚本不要带 `DETACHED_PROCESS`**。Windows PowerShell 5.1 在"没有控制台"的
  脱离进程里根本起不来：同样参数下，带 DETACHED 的那次连脚本第一行都没执行
  （`update.log` 空白，看起来像"脚本起了但什么都没干"），去掉就正常跑完。隐藏窗口
  靠 `HideWindow` + `-WindowStyle Hidden` 就够。另外补一层
  `CREATE_BREAKAWAY_FROM_JOB`：进程被塞进"关句柄就杀光"的作业对象时（IDE、部分
  终端、自动化宿主）子进程会被连带杀掉，先试脱离、被拒绝就退回不带该标志的启动。

装前先验安装目录可写（建一个探针文件再删）：放在 `Program Files` 里又没提权时，
宁可当场说"用管理员身份运行"，也不要下载完十几兆再在覆盖那一步失败。
运行在 `baozi.debug.exe` 上时拒绝自更新 —— 按三个固定文件名覆盖等于把调试构建
换成发布版，太容易误解。

除了"关于"页那个按钮，还有一条无窗口的入口 `baozi.exe --update-now`
（`cmd/baozi/updatecli.go`）：读配置（含"使用代理"）→ 查一次 → 有新版就直接走
同一段 `applyUpdate` → 退出，覆盖和重启照样由脚本完成。给端到端验收、快捷方式和
计划任务用；正常双击启动不带参数，完全走不到这条分支。两条路共用同一段代码，
免得只有界面那条会被修好、命令行那条慢慢漂掉。

## mygo 补丁：改了哪十一处，为什么不能放回本项目

`go.mod` 里是 `replace github.com/egoist/mygo => ./.patched-mygo`：仓库里放一份
上游 `v0.8.1` 的源码副本，只参与本项目编译；`go/pkg/mod` 里的上游和 `go.sum`
都没动，别的项目不受影响。

副本里相对上游只有下面十一处改动。`tools/make-mygo-patch.ps1` 会把它们导成
`patches/mygo-v0.8.1.patch`；带 `-Verify` 跑一遍会把这个 patch 应用到一个干净的
上游副本上，再逐字节比对，确认"文件里写的"就是"实际在编的"。

补丁已经提到上游：[egoist/mygo#2130](https://github.com/egoist/mygo/pull/2130)
（分支 `l0rraine:win10-glass-corners`，基于上游 `main`，三个提交）。合了就可以把
`replace` 换回上游版本、删掉这份副本。

| 文件 | 改动 | 不放回本项目的原因 |
|---|---|---|
| `internal/gpu/d3d11/d3d11.go` | 内容背景 alpha < 255 时不再强行拉成 1 | 在渲染器给交换链铺底色的那一步；外部拿不到这一帧 |
| `internal/windows/compositor.go` | `roundPixels`：按半径对整帧做像素级圆角，弧上带抗锯齿 | DirectComposition 的几何裁剪是硬边；像素在这一层，外部改不了 |
| `internal/windows/surface.go` | 内容子窗口加 `WS_EX_NOREDIRECTIONBITMAP`；子窗口回答 `WM_NCHITTEST` 并把 `WM_NCLBUTTONDOWN` 转发给顶层 | 子窗口由 mygo 创建，WndProc 是它私有的；外部只能改样式，接不住这些消息 |
| `internal/windows/window.go` | `WM_NCCALCSIZE` 让客户区铺满整个窗口；`resizeHitTest` 自己算四条边和四角 | 客户区尺寸由 mygo 的窗口过程算；子类化再改一层，等于和它抢同一批消息 |
| `internal/windows/window.go` | `WindowOptions.CornerRadius` 记进 window，供合成器取用 | 半径要在建窗口时落进内部状态；0 表示直角、不做内容裁剪 |
| `ui/frame.go`、`ui/services.go`、`ui/runtime.go`、`ui/window.go`、`content.go` | `FrameNow()`：立刻出帧、不等下一次刷新 | 出帧节拍在 host 的 `PacedSurface` 里；外面只能 `Invalidate`，那正是差一帧的来源 |
| `ui/headless.go` | 无窗口模式的 `frameNow`，以及 `Tester.Drags()` 计数 | 测试钩子，跟着上面那条走 |
| `ui/widgets.go` | `stylePanel`：`Theme.Background` 透明时浮层底板退回 `t.Surface`；`Select` 选中项改用 `SurfaceHover` | 浮层底板是 `ui` 内部画的；本项目把 `Theme.Background` 设成真透明（圆角四角要露桌面），弹层照着它画就成"能看穿的菜单" |

`FrameNow` 是拖动玻璃时"内容和窗口一起走"的关键：只 `Invalidate` 的话，窗口移动
落在下一帧、新内容再落一帧，那一帧的差就是"玻璃追不上窗口"。

早先这里还留过两处调试代码：首帧往 `%TEMP%\mygo-show.txt`、`mygo-round.txt`
写诊断，以及 `radius<=0` 时按 30px 兜底。后者会让传 `CornerRadius: 0` 的设置窗、
互传窗也被切 30px 圆角——那是验证阶段的常量，不是设计，已经删掉；现在 0 就是直角。



