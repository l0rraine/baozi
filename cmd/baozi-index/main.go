// baozi-index 是 Baozi 的索引服务：直接读 NTFS 的 USN Journal 维护索引，
// 不遍历目录。它比主程序多出来的能力只有一个——**能在提权下打开卷句柄**，
// 所以主程序自己不需要管理员权限。
//
// 用法（平时由托盘/设置页调起，不手敲）：
//
//	baozi-index.exe --install --data-dir "<用户数据目录>"   装成 Windows 服务并立即启动
//	baozi-index.exe --uninstall                             停止并删除服务
//	baozi-index.exe --once --data-dir "<dir>"               跑一轮建索引就退出（调试）
//	baozi-index.exe                                          控制台模式（前台跑，调试用）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"baozi/internal/config"
	"baozi/internal/index"
	"baozi/internal/usn"
)

const serviceName = "BaoziIndex"

const (
	// readPollInterval：多久读一次增量。文件变化最多延迟这么久被看见。
	// 空闲时这里只花一次系统调用（加上一次 2 秒的 sleep），不占 CPU。
	readPollInterval = 2 * time.Second
	// deltaFlushInterval：多久把攒下的增量写给主进程。
	// 主进程那边自己还有轮询节奏，这里 5 秒足够做到"敲完文件马上能搜到"，
	// 又不会把磁盘写忙。
	deltaFlushInterval = 5 * time.Second
	// compactDead / compactNames：攒到这个程度就做一次全量把它收掉。
	// 墓碑会让搜索多跳几次指针，arena 垃圾是纯浪费内存，两个都要有上限。
	compactDead  = 20000
	compactNames = 8 << 20
	// compactOps：一个 delta 最多攒这么多条指令。delta 是要被主进程整份读、
	// 整份重放的，不能无限长（否则每次合并都是几十毫秒）。
	compactOps = 5000
	// fullRebuildMaxAge：兜底。增量路径万一漏记/错记，定期全量能把它纠正回来；
	// 顺便重写 index.bin，把历史增量彻底清成一代。
	fullRebuildMaxAge = 6 * time.Hour
)

func main() {
	var (
		dataDir   string
		install   bool
		uninstall bool
		once      bool
		service   bool
	)
	flag.StringVar(&dataDir, "data-dir", "", "数据目录（默认 %LOCALAPPDATA%\\Baozi）")
	flag.BoolVar(&install, "install", false, "安装并启动服务（需要管理员）")
	flag.BoolVar(&uninstall, "uninstall", false, "停止并删除服务（需要管理员）")
	flag.BoolVar(&once, "once", false, "只建一轮索引就退出")
	flag.BoolVar(&service, "service", false, "以 Windows 服务方式运行（由 SCM 传入）")
	flag.Parse()

	if dataDir != "" {
		os.Setenv("BAOZI_DATA_DIR", dataDir)
	}

	dir, err := config.Dir()
	if err != nil {
		fail("数据目录不可用: %v", err)
	}
	setupLog(dir)

	switch {
	case install:
		cmdInstall(dir)
	case uninstall:
		cmdUninstall()
	case once:
		runOnce()
	case service:
		runService()
	default:
		if running, _ := svc.IsWindowsService(); running {
			runService()
			return
		}
		// 控制台模式：后台跑，Ctrl+C 结束，方便观察日志
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		log.Printf("索引服务（控制台模式）启动，数据目录 %s", dir)
		runIndexLoop(ctx)
	}
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// 服务模式下 stderr 没人看，尽量写日志
	log.Printf("失败: %s", msg)
	if running, _ := svc.IsWindowsService(); !running {
		fmt.Fprintln(os.Stderr, msg)
	}
	os.Exit(1)
}

func setupLog(dir string) {
	path := filepath.Join(dir, "baozi-index.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 8<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetOutput(f)
}

// ---------------------------------------------------------------- 服务安装

func cmdInstall(dataDir string) {
	exe, err := os.Executable()
	if err != nil {
		fail("取自身路径: %v", err)
	}
	exe, _ = filepath.Abs(exe)

	m, err := mgr.Connect()
	if err != nil {
		fail("连服务管理器（需要管理员）: %v", err)
	}
	defer m.Disconnect()

	// ChangeServiceConfig 不吃零值：ServiceType/ErrorControl 必须给实值，
	// 否则已装过的机器一升级就报 "The parameter is incorrect"（CreateService 容错，更新不容错）。
	cfg := mgr.Config{
		ServiceType:  windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:    mgr.StartAutomatic,
		ErrorControl: windows.SERVICE_ERROR_NORMAL,
		DisplayName:  "Baozi 索引服务",
		Description:  "直接读取 NTFS USN Journal 维护 Baozi 的文件索引，主程序因此不需要管理员权限。",
	}

	// 已经装过就更新 binPath（升级后 exe 换了位置也能自愈）。
	if s, err := m.OpenService(serviceName); err == nil {
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			fail("更新服务配置: %v", err)
		}
		s.Close()
	}

	args := []string{"--service", "--data-dir", dataDir}
	s, err := m.CreateService(serviceName, exe, cfg, args...)
	if err != nil {
		// 已存在就退而求其次：按名字打开再启动
		s, err = m.OpenService(serviceName)
		if err != nil {
			fail("创建服务: %v", err)
		}
	}
	defer s.Close()

	if err := setFailureActions(s); err != nil {
		// 配不上恢复动作不是致命错误（旧系统/权限怪），但要留痕：
		// 这正是"服务崩了没人拉起来、索引悄悄停在几天前"的成因。
		log.Printf("设置失败恢复动作失败（服务崩了不会自动重启）: %v", err)
	} else {
		log.Printf("失败恢复动作已设置: 5s/15s/60s/5min/15min 重启，计数器每日清零")
	}

	if err := s.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
		fail("启动服务: %v", err)
	}
	log.Printf("服务已安装并启动: %s → %s", serviceName, exe)
	fmt.Println("Baozi 索引服务已安装并启动")
}

// scAction 对应 Win32 的 SC_ACTION。
type scAction struct {
	Type  uint32
	Delay uint32 // 毫秒
}

// serviceFailureActions 对应 Win32 的 SERVICE_FAILURE_ACTIONSW。
// 字段顺序必须和 SDK 一致：ResetPeriod、RebootMsg、Command、cActions、lpsaActions。
type serviceFailureActions struct {
	ResetPeriod  uint32
	RebootMsg    *uint16
	Command      *uint16
	ActionsCount uint32
	Actions      uintptr
}

const (
	scActionRestart = 1
	// ChangeServiceConfig2 的信息级别
	scConfigFailureActions     = 2
	scConfigFailureActionsFlag = 4
)

var procChangeServiceConfig2 = windows.NewLazySystemDLL("advapi32.dll").
	NewProc("ChangeServiceConfig2W")

// setFailureActions 让 SCM 在服务异常退出后自动把它拉起来。
//
// 为什么必须由安装程序写，而不是让用户手敲 sc failure：
// 默认安装出来的服务 recovery actions 是空的（本机 `sc qfailure` 实测 RESET_PERIOD 0、
// 无动作），服务一旦意外停止（事件 7034）就永远躺着，主程序只能看到心跳变旧，
// 索引从此不再更新。用户装完不该还要懂 SCM。
//
// 五档 5s / 15s / 60s / 5min / 15min：前两次快速回来，之后放缓，既扛得住
// 开机时盘没就绪这类短暂故障，又不会在配置本身坏了的时候变成忙重启；
// 计数器 86400 秒清零，第二天重新给五次机会。
// 同时置 FailureActionsFlag=TRUE，让"非崩溃的异常退出"（返回非零退出码）也触发恢复——
// 否则服务自己 return 掉的路径（比如开机时盘还没就绪、打开卷失败）不会重启。
func setFailureActions(s *mgr.Service) error {
	acts := []scAction{
		{Type: scActionRestart, Delay: 5000},
		{Type: scActionRestart, Delay: 15000},
		{Type: scActionRestart, Delay: 60000},
		{Type: scActionRestart, Delay: 300000},
		{Type: scActionRestart, Delay: 900000},
	}
	sa := serviceFailureActions{
		ResetPeriod:  86400,
		ActionsCount: uint32(len(acts)),
		Actions:      uintptr(unsafe.Pointer(&acts[0])),
	}
	r1, _, err := procChangeServiceConfig2.Call(uintptr(s.Handle),
		scConfigFailureActions, uintptr(unsafe.Pointer(&sa)))
	runtime.KeepAlive(acts)
	if r1 == 0 {
		return err
	}
	flag := int32(1)
	r1, _, err = procChangeServiceConfig2.Call(uintptr(s.Handle),
		scConfigFailureActionsFlag, uintptr(unsafe.Pointer(&flag)))
	if r1 == 0 {
		return err
	}
	return nil
}

func cmdUninstall() {
	m, err := mgr.Connect()
	if err != nil {
		fail("连服务管理器（需要管理员）: %v", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println("服务不存在，无需卸载")
		return
	}
	defer s.Close()
	_, err = s.Control(svc.Stop)
	if err != nil && err != windows.ERROR_SERVICE_NOT_ACTIVE {
		log.Printf("停止服务: %v", err)
	}
	// 等服务真的停下来再删，否则 Delete 会被拒
	for i := 0; i < 20; i++ {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := s.Delete(); err != nil {
		fail("删除服务: %v", err)
	}
	fmt.Println("Baozi 索引服务已卸载")
}

// ---------------------------------------------------------------- 服务主体

type serviceHandler struct{}

func (serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var stopRequested atomic.Bool
	go func() {
		defer close(done)
		runIndexLoop(ctx)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for {
		select {
		case c, ok := <-r:
			if !ok {
				// SCM 那边断了（注销/关机），当成正常停止
				cancel()
				<-done
				return false, 0
			}
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				stopRequested.Store(true)
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case <-done:
			// 循环自己退了（打开卷失败、读不到配置……）。以前这里会一直挂着
			// "正在运行"的状态，进程活着但索引再也不动，主程序只看到心跳变旧。
			// 现在明确报停并以退出码 1 结束，让 SCM 的失败恢复动作把它拉起来。
			if stopRequested.Load() {
				return false, 0
			}
			cancel()
			changes <- svc.Status{State: svc.StopPending}
			return false, 1
		}
	}
}

func runService() {
	if err := svc.Run(serviceName, serviceHandler{}); err != nil {
		fail("服务运行: %v", err)
	}
}

func runOnce() {
	cfg, _, err := config.Load()
	if err != nil {
		fail("读配置: %v", err)
	}
	vols, err := openVolumes(cfg.Index.Roots)
	if err != nil {
		fail("打开卷: %v", err)
	}
	defer closeVolumes(vols)
	dataDir, derr := config.Dir()
	if derr != nil {
		fail("数据目录: %v", derr)
	}
	if _, _, err := rebuild(vols, cfg, dataDir, true); err != nil {
		fail("建索引: %v", err)
	}
}

// ruleFor 把配置里的收录规则，加上"数据目录收录但不跟增量"这一条。
//
// 为什么必须加：服务每 5 秒写一次 index.delta（还有 .tmp 改名、heartbeat、
// progress），每个动作都有 USN 记录。不冻结它，服务就会把自己的写入
// 当成"用户动了文件"再索引一遍，自己喂自己。
//
// 但它**不能排除**：用户要搜得到自己的 config.json 和 baozi.log（Everything
// 能搜到自己的 Everything.db 也是这个道理）。所以走"全量收录 + 增量冻结"：
// 建索引时它照常进来，之后对它的变化一律丢弃 —— 代价是这些条目的修改时间
// 停在上一次全量重建那一刻。
//
// 配置里的默认规则已经带了 %LOCALAPPDATA%\Baozi，但用户可以把数据目录
// 指到别处（--data-dir），所以这里再按绝对路径补一道。
func ruleFor(cfg *config.Config, dataDir string) (config.Index, index.Rule) {
	ic := cfg.Index
	ic.Frozen = append(append([]string{}, ic.Frozen...), dataDir)
	return ic, index.Rule{
		Excludes:      ic.Excludes,
		Freeze:        ic.Frozen,
		IncludeHidden: ic.IncludeHidden,
	}
}

// runIndexLoop 是服务的核心：首轮全量建索引，之后**每一条 USN 记录都直接改内存表**。
//
// 这条路径以前是"增量只当触发器"——因为索引是定长记录 + arena + 父节点存下标的
// 快照，逐条增删会破坏下标。结果就是每攒一批变化就整表重扫一次（15 秒 × 一天 1500 次
// = 7 小时 CPU，任务管理器里常年 8%）。
//
// 现在改法（和 Everything 一样的思路）：
//   - 新增条目 append 到末尾，已有下标一个都不动；
//   - 删除只标墓碑，槽位留着——删目录时子条目不产生 USN 记录，父子链必须留着
//     才能让那几千个子条目跟着失效；
//   - 全量只在"墓碑/垃圾攒够"和"兜底定期"时发生。
//
// 每次变化都会攒成一条 DeltaOp 写进 index.delta，主进程拿它把自己的快照同步过去，
// 于是索引整体不再有"5 分钟才更新一次"的窗口。
func runIndexLoop(ctx context.Context) {
	// 后台活：整表重建会连着吃十几秒 CPU。降到 Below Normal 让前台程序优先，
	// 重建慢个两三秒无所谓，但不能在用户敲字/编译的时候抢核。
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)

	cfg, _, err := config.Load()
	if err != nil {
		log.Printf("读配置失败: %v", err)
		return
	}
	vols, err := openVolumes(cfg.Index.Roots)
	if err != nil {
		log.Printf("打开卷失败: %v", err)
		return
	}
	// 闭包形式，roots 改了重开卷时能关掉最新那批句柄
	defer func() { closeVolumes(vols) }()
	log.Printf("索引服务就绪，卷 %v", letters(vols))

	dataDir, _ := config.Dir()

	// 首轮：建全量、落盘、同时拿到 FRN 映射（之后按 FRN 找条目全靠它）
	live, base, err := rebuild(vols, cfg, dataDir, true)
	if err != nil {
		log.Printf("首轮建索引失败: %v", err)
		return
	}
	st := loadState(dataDir)
	st.Base = base

	_, rule := ruleFor(cfg, dataDir)
	ops := make([]index.DeltaOp, 0, 1024)
	var seq uint64
	lastFlush := time.Now()
	lastFull := time.Now()
	lastCfg := time.Now()
	// 空闲判定：最近一次"真的有变化"的时刻，以及最近一次交回工作集的时刻。
	lastChangeAt := time.Now()
	lastTrimAt := time.Now()
	// rejectOnce 让"读到但没用上"的详单只打一次，免得刷满日志。
	rejectOnce := false
	// lastRejectLog 给那条汇总行做限流（一分钟一行）。
	var lastRejectLog time.Time

	// flush 把攒下的增量写盘。主进程读它把自己的快照追平。
	flush := func() {
		if len(ops) == 0 {
			return
		}
		seq++
		d := &index.Delta{Base: base, Seq: seq, Ops: ops}
		if err := index.WriteDelta(deltaPath(dataDir), d); err != nil {
			log.Printf("写增量失败: %v", err)
			seq--
			return
		}
		lastFlush = time.Now()
		// 位点也跟着推进：服务崩了重启，从这里接着读，不用倒回去重放几百万条
		writeState(dataDir, st.Volumes, live.Table().Len(), base)
	}

	// reset 把增量状态清成新一代（全量重建之后 delta 作废，下标基准变了）
	reset := func() {
		ops = ops[:0]
		seq = 0
		lastFlush = time.Now()
	}

	for {
		if ctx.Err() != nil {
			log.Printf("索引服务收到停止信号，退出")
			return
		}
		// 主程序点了"立即重建索引"会放这个信标（服务没有 IPC 端口，用文件最省事）
		if _, err := os.Stat(filepath.Join(dataDir, "index.rebuild")); err == nil {
			_ = os.Remove(filepath.Join(dataDir, "index.rebuild"))
			log.Printf("收到重建请求")
			// 重建前重新读配置：用户点这个按钮往往就是刚改完排除规则
			if c, _, e := config.Load(); e == nil {
				cfg = c
				if vols, e = reopenIfNeeded(vols, cfg.Index.Roots); e != nil {
					log.Printf("重开卷失败: %v", e)
				}
			}
			_, rule = ruleFor(cfg, dataDir)
			if nl, nb, e := rebuild(vols, cfg, dataDir, false); e != nil {
				log.Printf("重建失败: %v", e)
			} else {
				live.Close() // 解开上一代的 .frn 映射，否则写新代次会被占用挡住
				live, base = nl, nb
			}
			st = loadState(dataDir)
			st.Base = base
			reset()
			lastFull = time.Now()
			continue
		}

		// 每卷读一小段增量，不等内核（见 usn.Read 的注释：内核等待会整轮卡死）。
		// 读到就立刻应用——这就是"实时"的全部。
		applied := 0
		read := 0
		var firstSample *index.Change
		for _, v := range vols {
			next, _, err := v.Read(st.nextUsn(v.Letter()), st.journalID(v.Letter()), 0,
				func(r *usn.Record) bool {
					read++
					c := r.Change(v.Letter())
					if firstSample == nil {
						cc := c
						firstSample = &cc
					}
					if op, ok := live.Apply(c, rule); ok {
						ops = append(ops, op)
						applied++
					}
					return true
				})
			if err != nil {
				log.Printf("读增量 %s: %v", v.Letter(), err)
				continue
			}
			st.setNext(v.Letter(), next)
		}
		// 读到了却一条都没用上：绝大多数情况下是"被排除目录里的变动"（Temp、数据目录自己
		// 的心跳文件等），属于正常噪音，所以一分钟最多记一行；第一次额外打一份详单，
		// 用来定位"父节点查不到 / 基座没挂上"这类真故障。
		if read > 0 && applied == 0 {
			if !rejectOnce {
				rejectOnce = true
				firstC := firstSample
				if firstC != nil {
					pIdx, pOK := live.LookupFRN(firstC.ParentFRN)
					log.Printf("增量诊断: 表 %d 项 / 基座 %d 条；样本 name=%q frn=%016x parent=%016x 父命中=%v 下标=%d 父种类=%v",
						live.Table().Len(), live.FRNBaseLen(), firstC.Name, firstC.FRN,
						firstC.ParentFRN, pOK, pIdx,
						func() index.Kind {
							if !pOK {
								return 0
							}
							return live.Table().KindOf(pIdx)
						}())
				}
			}
			if time.Since(lastRejectLog) >= time.Minute {
				lastRejectLog = time.Now()
				log.Printf("增量: 读到 %d 条 USN 记录，但一条都没用上", read)
			}
		}
		if applied > 0 && time.Since(lastFlush) >= deltaFlushInterval {
			log.Printf("增量 %d 条已应用（本代累计 %d 条）", applied, len(ops))
			flush()
		}
		if applied > 0 {
			lastChangeAt = time.Now()
		}

		// 收拾的必要性：墓碑让搜索多跳指针，arena 追加是纯占内存，
		// 增量条数太多则主进程每次合并都会变慢。任何一条到线就整表重扫一次。
		needCompact := live.Compact(compactDead, compactNames) || len(ops) >= compactOps
		if needCompact {
			log.Printf("整理索引（墓碑 %d、名字追加 %d 字节、增量 %d 条）",
				live.Dead(), live.NamesOverhead(), len(ops))
		} else if time.Since(lastFull) >= fullRebuildMaxAge {
			log.Printf("定期全量重建（兜底，距上次 %v）", time.Since(lastFull).Round(time.Minute))
			needCompact = true
		}
		if needCompact {
			if nl, nb, e := rebuild(vols, cfg, dataDir, false); e != nil {
				log.Printf("重建失败: %v", e)
			} else {
				live.Close() // 解开上一代的 .frn 映射，否则写新代次会被占用挡住
				live, base = nl, nb
				st = loadState(dataDir)
				st.Base = base
				reset()
				lastFull = time.Now()
			}
		}

		// 每轮都喘一口：文件变化最多延迟这么久被看见，空闲时这里只是 sleep。
		// 不放在别处——持续有变化时会变成忙转，CPU 反而更难看。
		time.Sleep(readPollInterval)
		writeHeartbeat(dataDir)

		// 空闲就把工作集交回系统（"空闲"＝这一小段时间一条增量都没有）。
		//
		// 这个进程的常驻大部分是**文件映射页**（索引本体 74MB + FRN 表 21MB），
		// 它们本来就是干净的、可以随时丢掉再读回来；只要进程一直活着，Windows
		// 不会主动去收一个"安静"的进程的页，于是任务管理器上就一直显示一百多兆。
		// 每 idleTrimInterval 交回一次，数字立刻回到十几兆，而下次真要用到时只是
		// 一次软缺页（同一次读盘的量，几十微秒）。
		if applied == 0 && time.Since(lastChangeAt) >= idleTrimInterval &&
			time.Since(lastTrimAt) >= idleTrimInterval {
			trimWorkingSet()
			lastTrimAt = time.Now()
		}

		// 配置每 5 分钟重新读一次（根目录/排除规则变了要尽快生效）
		if time.Since(lastCfg) >= 5*time.Minute {
			lastCfg = time.Now()
			if c, _, err := config.Load(); err == nil {
				if rootsChanged(cfg.Index.Roots, c.Index.Roots) {
					log.Printf("索引范围变了，重建")
					cfg = c
					if vols, err = reopenIfNeeded(vols, cfg.Index.Roots); err != nil {
						log.Printf("重开卷失败: %v", err)
					}
					_, rule = ruleFor(cfg, dataDir)
					if nl, nb, e := rebuild(vols, cfg, dataDir, false); e != nil {
						log.Printf("重建失败: %v", e)
					} else {
						live.Close() // 解开上一代的 .frn 映射，否则写新代次会被占用挡住
						live, base = nl, nb
						st = loadState(dataDir)
						st.Base = base
						reset()
						lastFull = time.Now()
					}
				} else {
					cfg = c
					_, rule = ruleFor(cfg, dataDir)
				}
			}
		}
	}
}

// idleTrimInterval 是"空闲多久就把工作集交回系统"。
const idleTrimInterval = 60 * time.Second

// procSetProcessWorkingSetSize 是 kernel32 的进程工作集接口。
var procSetProcessWorkingSetSize = windows.NewLazySystemDLL("kernel32.dll").
	NewProc("SetProcessWorkingSetSize")

// trimWorkingSet 把可回收的页交回系统。
// 调用时传 -1/-1：由系统按当前可用内存决定，效果等同于 EmptyWorkingSet ——
// 把这些只读文件映射（索引本体 + FRN 表）从工作集里摘出去。页不会被丢弃，
// 只是回到 standby 列表，下次访问是软缺页（几十微秒）。
func trimWorkingSet() {
	if procSetProcessWorkingSetSize.Find() != nil {
		return
	}
	// 伪句柄 -1 就是当前进程，省掉 GetCurrentProcess 的两个返回值。
	procSetProcessWorkingSetSize.Call(^uintptr(0), ^uintptr(0), ^uintptr(0))
}

// rootsChanged 判断索引根目录集合是否变了。
func rootsChanged(a, b []string) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if !strings.EqualFold(strings.TrimRight(a[i], `\/`), strings.TrimRight(b[i], `\/`)) {
			return true
		}
	}
	return false
}

// reopenIfNeeded 在根目录集合变了的时候重开卷句柄（比如用户新加了 D 盘）。
func reopenIfNeeded(vols []*usn.Volume, roots []string) ([]*usn.Volume, error) {
	old := letters(vols)
	fresh, err := openVolumes(roots)
	if err != nil {
		return vols, fmt.Errorf("%v（沿用原来的卷 %v）", err, old)
	}
	nl := letters(fresh)
	if len(old) == len(nl) {
		same := true
		for i := range old {
			if !strings.EqualFold(old[i], nl[i]) {
				same = false
				break
			}
		}
		if same {
			closeVolumes(fresh) // 集合没变，新开的多余
			return vols, nil
		}
	}
	closeVolumes(vols)
	log.Printf("卷已更新: %v → %v", old, nl)
	return fresh, nil
}

// rebuild 全量枚举 → 落盘 → 写位点 → 返回**可继续增量维护的活表**。
//
// 和以前不同的是它现在把表留在内存里（以前 save 完就 tab=nil + FreeOSMemory，
// 只把 index.bin 当唯一真相）。留下它是增量的前提：新条目直接 append 上去，
// 搜到的就是最新状态，不必等下一次重扫。
func rebuild(vols []*usn.Volume, cfg *config.Config, dataDir string, first bool) (*index.Mutable, uint64, error) {
	// 构建期用默认 GC 目标：这 15 秒里分配量有 700MB 量级，压太低会一直 GC 反而变慢。
	debug.SetGCPercent(100)
	start := time.Now()
	ic, _ := ruleFor(cfg, dataDir)
	tab, frn, prog, err := usn.BuildAll(vols, ic, func(n int) {
		// 建的过程中每 10 万项打一行，别把日志刷成几千行
		if n%100000 < 20000 {
			log.Printf("建索引中… %d 项", n)
		}
	})
	if err != nil {
		return nil, 0, err
	}
	tSave := time.Now()
	binPath, err := index.Save(filepath.Join(dataDir, "index.bin"), tab, cfg.Index.Roots)
	if err != nil {
		return nil, 0, fmt.Errorf("保存索引: %w", err)
	}
	if os.Getenv("BAOZI_IDXPROFILE") != "" {
		log.Printf("建索引分段: 落盘 %v", time.Since(tSave).Round(time.Millisecond))
	}
	total := tab.Len()
	// 这一代的标识：主进程靠它判断"我载入的 bin 和这份 delta 是不是同一代"。
	// 用纳秒时间戳，单调且不会和上一代撞。
	base := uint64(time.Now().UnixNano())
	log.Printf("索引已更新: %d 项，耗时 %v", total, time.Since(start).Round(100*time.Millisecond))
	writeState(dataDir, prog, total, base)
	// 新的一代：旧 delta 的 Base 已经不匹配，直接清掉（主进程看到空 delta 会重新载入 bin）
	if err := index.WriteDelta(deltaPath(dataDir), &index.Delta{Base: base}); err != nil {
		log.Printf("清空增量失败: %v", err)
	}

	// 落盘就绪后，把这张 176 万条的堆表换成刚写出的文件的 mmap 基座。
	//
	// 堆表是构建期的产物：Nodes 切片 + 名字 arena + 排序临时数组，实测私有提交
	// 311MB，而且因为 Mutable 一直握着它，FreeOSMemory 也回收不掉——这是
	// baozi-index 长期占 300MB 的唯一原因。服务每次启动都会重扫一遍全量，
	// 留着它没有任何后续用途。
	//
	// 换成只读基座后语义不变：基座只读、增量全部走 overlay（appendOverlay /
	// markDead），和主进程那边是同一套结构。FRN → 下标的映射在 BuildAll 里已经
	// 建好，而落盘顺序就是内存下标顺序，所以下标可以直接复用，不必把 FRN 落盘
	// 进 bin —— 但那张堆表本身要落成旁挂文件（见下），否则它就是这个进程里
	// 最大的一块常驻内存。
	tSave = time.Now()
	tab = nil
	mapped, _, err := index.Load(binPath)
	if err != nil {
		return nil, 0, fmt.Errorf("回映射索引: %w", err)
	}
	// 把 177 万对 (FRN, 下标) 排好序写成 index-<代>.frn，再映射回来当基座：
	// 那张 30MB 的开放寻址表（2.6M 槽 × 12 字节）换成 file-backed 页。
	// 写不出来就退回堆上那份 —— 宁可多占 30MB，也不能让增量查不到 FRN。
	var mut *index.Mutable
	frnPath, ferr := index.SaveFRNFile(index.FRNPathFor(binPath), frn)
	if ferr != nil {
		log.Printf("FRN 表落盘失败（退回堆表 %d 条）: %v", frn.Len(), ferr)
		mut = index.NewMutable(mapped, frn)
	} else if ff, lerr := index.LoadFRNFile(frnPath); lerr != nil {
		log.Printf("FRN 表映射失败（退回堆表 %d 条）: %v", frn.Len(), lerr)
		mut = index.NewMutable(mapped, frn)
	} else {
		frn = nil // 堆表到此可以回收
		mut = index.NewMutableBase(mapped, ff)
	}
	// 稳态把 GC 目标压到 30%：FRN 改成映射后活堆只剩增量尾巴和拼音表（几 MB），
	// 默认 100% 会让 Go 留着几倍的空闲区不放。服务平时几乎不分配，压低目标
	// 只多几次可忽略的 GC，却能把这几十 MB 直接减掉。先设目标再 FreeOSMemory，
	// 归还量才按新目标算。
	debug.SetGCPercent(30)
	debug.FreeOSMemory()
	if os.Getenv("BAOZI_IDXPROFILE") != "" {
		log.Printf("建索引分段: 回映射 %v", time.Since(tSave).Round(time.Millisecond))
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	log.Printf("索引常驻: 活堆 %d MB / 向系统要了 %d MB / 已归还 %d MB / FRN %d 条（基座 %d 映射 + 增量 %d 堆）",
		ms.HeapAlloc/(1<<20), ms.HeapSys/(1<<20), ms.HeapReleased/(1<<20),
		mut.FRNCount(), mut.FRNBaseLen(), mut.FRNOvLen())
	// BAOZI_IDXHEAP=<路径> 时落一份堆快照，用来回答"这几十 MB 到底是谁占的"。
	if p := os.Getenv("BAOZI_IDXHEAP"); p != "" {
		if f, e := os.Create(p); e == nil {
			_ = pprof.WriteHeapProfile(f)
			f.Close()
			log.Printf("堆快照已写入 %s", p)
		}
	}
	// 注意返回的是 mut：它要么握着映射基座（正常路径），要么握着堆表（落盘失败时的退路）。
	// 这里绝不能重新 NewMutable(mapped, frn)——frn 成功落盘时已经被置 nil，
	// 那样返回的 Mutable 就一个 FRN 也查不到，所有增量会被静默丢弃。
	return mut, base, nil
}

// ---------------------------------------------------------------- 卷与状态文件

func openVolumes(roots []string) ([]*usn.Volume, error) {
	var lettersSet []string
	seen := map[string]bool{}
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if len(r) < 2 || r[1] != ':' {
			continue
		}
		l := strings.ToUpper(r[:1])
		if seen[l] {
			continue
		}
		seen[l] = true
		lettersSet = append(lettersSet, l)
	}
	if len(lettersSet) == 0 {
		return nil, fmt.Errorf("配置里没有可用的盘符根目录")
	}
	var out []*usn.Volume
	for _, l := range lettersSet {
		v, err := usn.Open(l)
		if err != nil {
			log.Printf("跳过卷 %s: %v", l, err)
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可打开的 NTFS 卷（需要管理员/服务权限）")
	}
	return out, nil
}

func closeVolumes(vols []*usn.Volume) {
	for _, v := range vols {
		v.Close()
	}
}

func letters(vols []*usn.Volume) []string {
	out := make([]string, 0, len(vols))
	for _, v := range vols {
		out = append(out, v.Letter())
	}
	return out
}

// state 是服务侧保存的增量位点，同时也是"服务活着"的证据。
type state struct {
	Volumes   []usn.Progress `json:"volumes"`
	Total     int            `json:"total"`
	Heartbeat time.Time      `json:"heartbeat"`
	// Base 是当前 index.bin 这一代的标识。主进程拿它和 delta 的 Base 比对，
	// 对不上（说明 bin 已经换代）就重新整份载入，而不是往上叠增量。
	Base uint64 `json:"base"`
}

func statePath(dataDir string) string     { return filepath.Join(dataDir, "index.progress.json") }
func heartbeatPath(dataDir string) string { return filepath.Join(dataDir, "index.heartbeat") }
func deltaPath(dataDir string) string     { return filepath.Join(dataDir, "index.delta") }

func loadState(dataDir string) *state {
	s := &state{}
	if data, err := os.ReadFile(statePath(dataDir)); err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *state) nextUsn(letter string) int64 {
	for _, p := range s.Volumes {
		if strings.EqualFold(p.Letter, letter) {
			return p.NextUsn
		}
	}
	return 0
}

func (s *state) journalID(letter string) uint64 {
	for _, p := range s.Volumes {
		if strings.EqualFold(p.Letter, letter) {
			return p.JournalID
		}
	}
	return 0
}

func (s *state) setNext(letter string, next int64) {
	for i := range s.Volumes {
		if strings.EqualFold(s.Volumes[i].Letter, letter) {
			if next > s.Volumes[i].NextUsn {
				s.Volumes[i].NextUsn = next
			}
			return
		}
	}
	s.Volumes = append(s.Volumes, usn.Progress{Letter: letter, NextUsn: next})
}

func writeState(dataDir string, prog []usn.Progress, total int, base uint64) {
	s := &state{Volumes: prog, Total: total, Heartbeat: time.Now(), Base: base}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := statePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("写位点失败: %v", err)
		return
	}
	_ = os.Rename(tmp, statePath(dataDir))
}

func writeHeartbeat(dataDir string) {
	now := time.Now()
	if st, err := os.Stat(heartbeatPath(dataDir)); err == nil && now.Sub(st.ModTime()) < time.Minute {
		return // 一分钟写一次就够主进程判断了
	}
	_ = os.WriteFile(heartbeatPath(dataDir), []byte(now.Format(time.RFC3339Nano)), 0o644)
}
