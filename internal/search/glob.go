// 通配符（glob）模式：`*.pdf`、`2026?年报*`、`[草稿]*.docx` 这类输入，
// 翻成正则再走**同一条**匹配路径（编译、字面预筛、逐条 Match 全部复用 regex.go）。
//
// 为什么不自己写一套 glob 匹配器：两百万条上逐条比对，代价和正则引擎是同一档，
// 而正则那条路已经有"从模式里求必现字面、先零分配子串筛一遍"的做法
// （`^.*\.log$` 从 647ms 降到 59ms）。翻译一遍就白捡这套预筛，不必再造一份。
//
// 口径按 Windows 的习惯来：
//
//   - `*` 任意串，`?` 单个字符，`[...]` 字符集（`[!abc]` 取反）；
//   - 整条模式锚定**整个文件名**：`*.pdf` 就是"以 .pdf 结尾"，不是"名字里含 .pdf"；
//   - 只匹配文件名，不匹配路径（和正则模式一致，理由见 searchRegex 的注释）；
//   - **不区分大小写**：资源管理器里 `*.PDF` 和 `*.pdf` 是同一件事，
//     搜索框里写哪个都该搜得到。用 `(?i)` 交给正则去折，预筛只拿 ASCII 字面
//     （见 literalScreen：非 ASCII 字面在忽略大小写时会折到别的码位，
//     拿它预筛有漏结果的风险）。
package search

import (
	"errors"
	"regexp"
	"strings"
)

// HasGlob 说这串查询是不是"通配符意图"：出现 `*` 或 `?` 就算。
//
// 只看这两个：`[` 单独出现太容易是文件名里真有个方括号（`[草稿]`、`[2026]`），
// 把它当通配符会把用户本来想按字面搜的查询改道。`[...]` 只在已经因为 `*`/`?`
// 走通配符这条路时才生效。
func HasGlob(q string) bool {
	return strings.ContainsAny(q, "*?")
}

// CompileGlob 把通配符编译成 Regex —— 类型和 CompileRegex 的完全一样，
// 调用方拿到之后照旧 Maybe/Match，不必知道自己走的是哪条路。
func CompileGlob(pattern string) (*Regex, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return nil, errors.New("空的通配符模式")
	}
	src, lits := globToRegexp(p)
	// (?i) 放在最前面，对整条模式生效（含字符集）。
	re, err := regexp.Compile("(?i)" + src)
	if err != nil {
		// 翻译出来的模式理应永远合法；真出错也照实说出来，不静默。
		return nil, err
	}
	return &Regex{re: re, screen: literalScreen(lits)}, nil
}

// globToRegexp 把通配符翻成"锚定整名"的正则源码，并顺手记下每段必现的字面。
//
// 返回的 lits 全是**一定出现**的片段：`*`/`?` 两边被切开的那几段字面，
// 每一段都必须命中（不是或的关系），所以可以直接拿来当预筛条件。
// 字符集不产生字面 —— 它一个字符能代表好几个，取哪段都不保险。
func globToRegexp(p string) (string, []string) {
	var (
		b    strings.Builder
		lits []string
		run  []rune
	)
	b.WriteString("^")
	flush := func() {
		if len(run) == 0 {
			return
		}
		s := string(run)
		b.WriteString(regexp.QuoteMeta(s))
		lits = append(lits, s)
		run = run[:0]
	}
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			// 反斜杠转义下一个字符：`\*` 就是要找名字里带星号的文件。
			if i+1 < len(rs) {
				i++
				run = append(run, rs[i])
			} else {
				run = append(run, '\\')
			}
		case '*':
			flush()
			b.WriteString(".*")
		case '?':
			flush()
			b.WriteString(".")
		case '[':
			flush()
			class, next, ok := globClass(rs, i)
			if !ok {
				// 没有闭合的 `]`：当普通字符，别让用户一个手滑就整条失效。
				run = append(run, '[')
				continue
			}
			b.WriteString(class)
			i = next
		default:
			run = append(run, rs[i])
		}
	}
	flush()
	b.WriteString("$")
	return b.String(), lits
}

// globClass 在 rs[at] == '[' 处解析一个字符集，返回正则片段和 `]` 的下标。
// 认 `[!abc]`（Windows 写法）和 `[^abc]`（正则写法）两种取反；`]` 紧跟在开头时是字面。
func globClass(rs []rune, at int) (string, int, bool) {
	var b strings.Builder
	b.WriteString("[")
	i := at + 1
	if i < len(rs) && (rs[i] == '!' || rs[i] == '^') {
		b.WriteString("^")
		i++
	}
	if i < len(rs) && rs[i] == ']' {
		b.WriteString(`\]`)
		i++
	}
	closed := false
	for ; i < len(rs); i++ {
		c := rs[i]
		if c == ']' {
			closed = true
			break
		}
		if c == '\\' && i+1 < len(rs) {
			i++
			c = rs[i]
		}
		// `-` 留给正则当范围分隔符：QuoteMeta 本来也不转义它，
		// 这里只是别把 `[a-z]` 的连字符拆成字面。
		if c == '-' {
			b.WriteByte('-')
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(c)))
	}
	if !closed {
		return "", 0, false
	}
	b.WriteString("]")
	return b.String(), i, true
}

// literalScreen 把必现字面整理成预筛条件：全部字面都得出现（AND）。
//
// 只留两字节以上、且**全是 ASCII** 的：短字面筛不出多少，白花一次扫描；
// 非 ASCII 字面在 `(?i)` 下会被 Go 做 Unicode 折叠，而预筛只折 ASCII 字节，
// 拿它筛可能漏掉本该命中的条目 —— 预筛宁可少省点 CPU，绝不能漏结果。
// 一段都留不下就不预筛（此时全表过正则，几百毫秒，但结果是对的）。
func literalScreen(lits []string) [][]string {
	var and []string
	for _, s := range lits {
		if len(s) < 2 || !isASCII(s) {
			continue
		}
		and = append(and, strings.ToLower(s))
	}
	if len(and) == 0 {
		return nil
	}
	return [][]string{and}
}
