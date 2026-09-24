package main

// fuzz_test.go —— 把清单解析器当**不可信输入**来撞。
//
// 以前的测试全是手写的"能想到的形状"。fuzz 的价值在于撞想不到的:截断、命名空间、
// BOM、非法 UTF-8、属性里塞控制字符。断言也不是"没崩就算过",而是契约不变量:
//   1. 返回的 path 必须在工作区内(不绝对、无 `..` 分量)      -- §3
//   2. 返回的 URL 必须过传输白名单                            -- §4
//   3. 返回的项目 revision 一定非空(不猜)                    -- §1
//   4. 拒绝时的消息必须说清问题(不能一句 "bad")              -- §0
//
// 跑法:
//   go test -run FuzzParseManifest            # 只跑种子(常规)
//   go test -fuzz FuzzParseManifest -fuzztime 30s

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// dieMarker 区分"工具正确地拒绝了坏清单"(受控 die)与真 bug(其他 panic)。
type dieMarker struct{ msg string }

// dieMu:parseSafely 要临时替换包级 die。fuzz worker 是并行的,不锁住就是数据竞争
// (这个坑已经踩过一次:见 archive.go 里 readIdleTimeout 的快照注释)。
var dieMu sync.Mutex

// parseSafely 在 die->panic 的替换下跑 fn,返回受控 die 的消息("" 表示没被拒)。
// 非受控 panic 一律原样抛出,让 go test 报成 crash。
func parseSafely(t testing.TB, fn func()) (msg string) {
	t.Helper()
	dieMu.Lock()
	defer dieMu.Unlock()
	origDie := die
	die = func(f string, a ...any) { panic(dieMarker{fmt.Sprintf(f, a...)}) }
	defer func() {
		die = origDie
		switch r := recover().(type) {
		case nil:
		case dieMarker:
			msg = r.msg
		default:
			panic(r)
		}
	}()
	fn()
	return ""
}

// childManifest 是固定的一份合法小清单:输入里写 <include name="child.xml"/> 就能
// 真正走到递归 include 那条路(种子和 fuzzer 都会发现它)。
const childManifest = `<manifest>
  <remote name="c" fetch="child/"/>
  <default revision="t1" remote="c"/>
  <project name="ckid" path="ckid"/>
</manifest>`

// fuzzWant 是 fuzz 输入的语义:任意字节都合法输入,所以"被拒绝"是允许的结果 ——
// 只要拒绝的理由说得出。手写 seed 才有"应当接受/应当因某条原因被拒"的预期。
const fuzzWant = "*"

func checkManifestText(t *testing.T, src string) { checkManifestTextWant(t, src, fuzzWant) }

// checkManifestTextWant 跑完整条只读管线(解析 + 解析成项目/归档),然后:
//   - wantErr 非空:必须被拒,且拒的原因要对(拒错地方说明 seed 写坏了或防线过严)
//   - wantErr 为空:必须被接受,并且返回值要满足三条不变量(path 在界内、URL 过白
//     名单、revision 非空)
//
// 两个阶段合在**一次** parseSafely 里:分开写会导致"解析成功、解析项目时被拒"的输入
// 被当成"意外接受"或让断言被悄悄跳过。
func checkManifestTextWant(t *testing.T, src, wantErr string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "default.xml"), src)
	writeFile(t, filepath.Join(dir, "child.xml"), childManifest)

	var m *Manifest
	var projects []Project
	var archives []Archive
	base := "https://host.example/pub/mf.git"
	msg := parseSafely(t, func() {
		m = parseManifestFile(dir, "default.xml")
		projects = resolveProjects(m, base)
		archives = resolveArchives(m, base)
	})
	finishWant(t, msg, wantErr, src)
	if msg != "" {
		return
	}
	for _, p := range projects {
		if p.Path == "" || strings.HasPrefix(p.Path, "/") || strings.Contains(p.Path, `\`) {
			t.Fatalf("project path is not workspace-relative: %q (input %q)", p.Path, brief(src))
		}
		for _, seg := range strings.Split(p.Path, "/") {
			if seg == ".." {
				t.Fatalf("project path keeps a '..' component: %q (input %q)", p.Path, brief(src))
			}
		}
		if p.Revision == "" {
			t.Fatalf("project %q came back with an empty revision -- contract says never guess", brief(src))
		}
		if k, _ := classifyURL(p.URL); k != "" {
			t.Fatalf("project %q survived with a rejected URL %q (kind %q)", p.Path, p.URL, k)
		}
		// 最终 URL 以 '-' 开头就是 git argv 注入的形状(--upload-pack=...)。
		// absolutize 把它锚到 remote fetch 下才算安全,这条断言钉住"锚了"。
		if strings.HasPrefix(p.URL, "-") {
			t.Fatalf("project %q resolved to an option-like URL %q", p.Path, p.URL)
		}
	}
	for _, a := range archives {
		if k, _ := classifyURL(a.URL); k != "" {
			t.Fatalf("archive survived with a rejected URL %q (kind %q)", a.URL, k)
		}
		if strings.HasPrefix(a.URL, "-") {
			t.Fatalf("archive resolved to an option-like URL %q", a.URL)
		}
		if a.Path == "" || strings.HasPrefix(a.Path, "/") {
			t.Fatalf("archive path is not workspace-relative: %q", a.Path)
		}
	}
}

// finishWant 统一收口:该接受的有没有被误拒、该拒的原因对不对。
func finishWant(t *testing.T, msg, wantErr, src string) {
	t.Helper()
	switch {
	case wantErr == fuzzWant: // 任意输入:拒可以,但要说清;接受则要过不变量(调用方查)
		if msg != "" {
			assertExplains(t, msg, src)
		}
	case wantErr == "": // 预期接受:被拒说明 seed 写坏了或某道防线过严
		if msg != "" {
			t.Fatalf("input meant to be accepted was refused: %q (input %q)", msg, brief(src))
		}
	default: // 预期因某条原因被拒
		if msg == "" {
			t.Fatalf("input must be refused as %q but was accepted (input %q)", wantErr, brief(src))
		}
		assertExplains(t, msg, src)
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(wantErr)) {
			t.Fatalf("refused for the wrong reason: want %q, got %q (input %q)", wantErr, msg, brief(src))
		}
	}
}

// assertExplains 只查一件事:被拒绝时,消息要能指出一条可行动的线索(元素名/属性名/
// 传输名/文件名),而不是把用户推回去查网络。
func assertExplains(t testing.TB, msg, src string) {
	t.Helper()
	if len(msg) < 20 {
		t.Fatalf("rejection is not explained (%q) for input %q", msg, brief(src))
	}
	if strings.Contains(msg, "%!") || strings.Contains(msg, "PANIC") {
		t.Fatalf("rejection message is broken: %q", msg)
	}
}

func brief(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= 160 {
		return s
	}
	return s[:160] + "..."
}

func TestManifestParserProperties(t *testing.T) {
	for _, tc := range seedManifests {
		t.Run(tc.name, func(t *testing.T) { checkManifestTextWant(t, tc.body, seedWant[tc.name]) })
	}
}

// TestIncludeBudget 打的是指数展开:每层把同一个 include 引用两次,20 层就是 100 万
// 次解析(实测 18 层 4.2s、22 层约 108s)。上限必须让它立刻干净失败,而不是让
// `minirepo list` 永久挂住。
func TestIncludeBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("<manifest>")
	for i := 0; i < 400; i++ {
		b.WriteString(`<include name="child.xml"/>`)
	}
	b.WriteString("</manifest>")

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "default.xml"), b.String())
	writeFile(t, filepath.Join(dir, "child.xml"), childManifest)

	done := make(chan string, 1)
	go func() { done <- parseSafely(t, func() { parseManifestFile(dir, "default.xml") }) }()
	var msg string
	select {
	case msg = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the include budget did not fire: this manifest hangs the tool forever")
	}
	if !strings.Contains(msg, "more than 256 files") {
		t.Errorf("must be stopped by the parse budget, got %q", msg)
	}
}

func FuzzParseManifest(f *testing.F) {
	for _, s := range seedManifests {
		f.Add(s.body) // wantErr 只用于手写表;fuzz 输入没有"预期",只查不变量
	}
	if fi, err := os.ReadFile("CONTRACT.md"); err == nil {
		f.Add(string(fi[:min(200, len(fi))])) // 真实文本:检验"根本不是 XML"这条
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 64*1024 {
			t.Skip("pathological input size; not interesting here")
		}
		checkManifestText(t, src)
	})
}
