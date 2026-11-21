package win

import "testing"

// shouldFollow 的三类场景：
//  1. 第一次见到框：只有刚从文件管理器切过来才跳；
//  2. 见过框、期间版本变过：即使紧邻的上一个前台是应用主窗（Alt+Tab 回来），也要跳；
//     版本号在"来源窗口重新成为前台"时就会 +1，所以只切到 Arke、没换目录也算变过；
//  3. 见过框、版本没变：不跳（保护用户手动在框里选的目录）。
func TestShouldFollow(t *testing.T) {
	cases := []struct {
		name          string
		seenVersion   int
		seenBefore    bool
		version       int
		prevWasSource bool
		want          bool
	}{
		{"第一次·从文件管理器来", 0, false, 3, true, true},
		{"第一次·从普通程序来", 0, false, 3, false, false},
		{"见过·版本变了·紧邻的是普通程序（Alt+Tab 回来）", 3, true, 4, false, true},
		{"见过·版本变了·上次还是文件管理器", 3, true, 4, true, true},
		{"见过·只切过来源、目录没变", 3, true, 4, false, true},
		{"见过·版本没变·上次是文件管理器", 4, true, 4, true, false},
		{"见过·版本没变·上次是普通程序", 4, true, 4, false, false},
	}
	for _, c := range cases {
		if got := shouldFollow(c.seenVersion, c.seenBefore, c.version, c.prevWasSource); got != c.want {
			t.Fatalf("%s: 期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

// 第一次没跳时也要把版本号记下来：否则同一个框之后不会再被当作"见过"，
// 用户去文件管理器换了目录再回来，仍然不会跳。
func TestDialogSeenBaseline(t *testing.T) {
	f := &DialogFollow{seen: map[uintptr]int{}}
	f.handleDialog(0x1234, false)
	if v, ok := f.dialogSeen(0x1234); !ok || v != 0 {
		t.Fatalf("跳过后应当记下基线，实际 ok=%v version=%d", ok, v)
	}
	// 版本升到 1 之后，同一个框按"见过且版本变了"处理。
	f.version = 1
	if !shouldFollow(0, true, 1, false) {
		t.Fatal("版本变过之后应当跟随")
	}
}
