// indexprobe 是个一次性诊断工具：按名字在 index.bin 里查条目，打印路径和时间。
// 用来验证"冻结目录里的文件到底进没进索引"，不用开面板敲字。
package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"baozi/internal/config"
	"baozi/internal/index"
)

func main() {
	dir, err := config.Dir()
	if err != nil {
		log.Fatalf("数据目录: %v", err)
	}
	path, _, err := index.Latest(dir)
	if err != nil {
		log.Fatalf("找索引: %v", err)
	}
	tab, roots, err := index.Load(path)
	if err != nil {
		log.Fatalf("载入 %s: %v", path, err)
	}
	query := strings.ToLower(strings.Join(os.Args[1:], " "))
	if query == "" {
		query = "baozi.log"
	}
	fmt.Printf("索引 %s：%d 项，根 %v\n", path, tab.Len(), roots)
	if n, err := strconv.Atoi(query); err == nil {
		fmt.Printf("下标 %d → %s\n", n, tab.PathOf(int32(n)))
		return
	}
	shown := 0
	for i := 0; i < tab.Len() && shown < 20; i++ {
		if tab.Dead(int32(i)) {
			continue
		}
		if !tab.Hits(i, []string{query}) {
			continue
		}
		name := tab.Name(i)
		hint := tab.Dir(int32(i))
		if strings.HasSuffix(strings.ToLower(hint), `\appdata\local\baozi`) ||
			strings.HasSuffix(strings.ToLower(name), ".log") {
			fmt.Printf("  %s\\%s  (改于 %s)\n", hint, name, tab.Age(*tab.At(int32(i))).Format("2006-01-02 15:04:05"))
			shown++
		}
	}
	if shown == 0 {
		fmt.Printf("没有命中 %q\n", query)
	}
}
