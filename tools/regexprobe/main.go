// regexprobe：用真实索引（170 万条）量正则模式的成本，
// 口径和产品里的 searchRegex 一致：search.CompileRegex → Maybe（字面预筛）
// → Match（regexp 引擎）→ 命中后还要 PathOf / Ext（这两步才是贵的）。
//
// 用法：go run ./tools/regexprobe '^.*\.log$'
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"baozi/internal/config"
	"baozi/internal/index"
	"baozi/internal/search"
)

// 产品里 searchRegex 的收集上限（cmd/baozi/main.go 的 maxRegexHits）。
const maxRegexHits = 20000

func main() {
	pattern := `^.*\.log$`
	glob := false
	if len(os.Args) > 1 && os.Args[1] == "-glob" {
		glob = true
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	if len(os.Args) > 1 {
		pattern = os.Args[1]
	}
	dir, err := config.Dir()
	if err != nil {
		fmt.Println("数据目录:", err)
		os.Exit(1)
	}
	path := filepath.Join(dir, "index.bin")
	start := time.Now()
	tab, _, err := index.Load(path)
	if err != nil {
		fmt.Println("加载索引:", err)
		os.Exit(1)
	}
	fmt.Printf("索引 %d 条，加载 %v\n", tab.Len(), time.Since(start).Round(time.Millisecond))

	var rx *search.Regex
	if glob {
		// 通配符模式：和 searchAll 走 searchGlob 时编译的是同一个东西
		rx, err = search.CompileGlob(pattern)
	} else {
		rx, err = search.CompileRegex(pattern)
	}
	if err != nil {
		fmt.Println("模式无效:", err)
		os.Exit(1)
	}

	// 1) 只算 Maybe：预筛本身的开销
	t0 := time.Now()
	screened := 0
	for i := 0; i < tab.Len(); i++ {
		if rx.Maybe(tab.NameBytes(i)) {
			screened++
		}
	}
	fmt.Printf("只预筛：放行 %d 条，耗时 %v\n", screened, time.Since(t0).Round(time.Millisecond))

	// 2) 预筛 + 正则匹配（searchRegex 的匹配段）
	t0 = time.Now()
	matched := 0
	for i := 0; i < tab.Len(); i++ {
		name := tab.NameBytes(i)
		if !rx.Maybe(name) || !rx.Match(name) {
			continue
		}
		matched++
	}
	fmt.Printf("预筛+正则：命中 %d 条，耗时 %v\n", matched, time.Since(t0).Round(time.Millisecond))

	// 3) 完整循环：再加命中后的 PathOf + Ext（一条结果真正要付的代价）
	t0 = time.Now()
	full, paths := 0, 0
	var first []string
	for i := 0; i < tab.Len(); i++ {
		name := tab.NameBytes(i)
		if !rx.Maybe(name) || !rx.Match(name) {
			continue
		}
		p := tab.PathOf(int32(i))
		_ = tab.Ext(i)
		paths += len(p)
		full++
		if len(first) < 3 {
			first = append(first, p)
		}
		if full >= maxRegexHits {
			break
		}
	}
	fmt.Printf("完整循环（含 PathOf/Ext，上限 %d）：%d 条，耗时 %v\n",
		maxRegexHits, full, time.Since(t0).Round(time.Millisecond))
	for _, p := range first {
		fmt.Println("  ", p)
	}
}
