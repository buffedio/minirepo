package main

// completion_exec_test.go —— 真的把补全函数跑起来断言 COMPREPLY。
//
// 静态对拍(guards_test.go)只能证明"宣传的选项存在",证明不了"函数体算得出正确
// 候选"。这里钉住的正是当初两类实际出错的地方:
//   - 项目名被补成 `kernel/`(带斜杠)而 sync 的过滤是精确匹配 -> 直接报 unknown project
//   - 选项的"值"补全写在 case "$cur" 里(--shell/-j/-b 永远匹配不上,只响铃)
// 没有 bash 就 skip(不是"没有就通过":跳过时会明确打印原因)。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireBash(t *testing.T) string {
	t.Helper()
	b, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available: completion cannot be executed, skipping (NOT a pass)")
	}
	return b
}

// compRun 模拟一次 TAB:返回 COMPREPLY。words 是完整命令行(第一个元素是程序名,
// 传测试二进制的绝对路径,脚本会用它去取项目列表)。
func compRun(t *testing.T, script, bin string, words []string, cwd string) []string {
	t.Helper()
	// 尾随空参数代表"光标停在一个空词上",bash 里就是这么给的
	list := make([]string, 0, len(words))
	list = append(list, words...)
	decl := ""
	for _, w := range list {
		decl += " " + shellQuote(w)
	}
	drv := `source ` + shellQuote(script) + `
COMP_WORDS=(` + strings.TrimSpace(decl) + `)
COMP_CWORD=$(( ${#COMP_WORDS[@]} - 1 ))
COMPREPLY=()
_minirepo >/dev/null 2>&1
printf '%s\n' "${COMPREPLY[@]}"
`
	cmd := exec.Command("bash", "-c", drv)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("completion driver failed: %v\n%s", err, out)
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" {
			res = append(res, l)
		}
	}
	return res
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, "' \t\"\\$`") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

// compFixture 建一个真工作区:两个项目(其中一个嵌套、名字里带斜杠)、清单仓带一个
// 非默认分支,便于断言 -b 的候选。
func compFixture(t *testing.T) (ws, script, bin string) {
	t.Helper()
	root := t.TempDir()
	up := filepath.Join(root, "up")
	mkRepo(t, filepath.Join(up, "top"))
	mkRepo(t, filepath.Join(up, "pkg", "nested"))
	mkRepo(t, filepath.Join(up, "ToolChain"))

	mf := filepath.Join(root, "mf")
	os.MkdirAll(mf, 0o755)
	writeFile(t, filepath.Join(mf, "default.xml"), `<manifest>
  <remote name="o" fetch="`+filepathToSlash(up)+`/" />
  <default remote="o" revision="master" sync-c="true" />
  <project name="top" path="top" />
  <project name="pkg/nested" path="pkg/nested" />
  <project name="ToolChain" path="ToolChain" />
</manifest>`)
	g(t, mf, "init", "-q", "--initial-branch=master")
	g(t, mf, "add", "-A")
	g(t, mf, "commit", "-q", "-m", "manifest")
	g(t, mf, "checkout", "-q", "-b", "release/v2") // 供 init -b 补全
	g(t, mf, "checkout", "-q", "master")

	ws = filepath.Join(root, "ws")
	os.MkdirAll(ws, 0o755)
	bin = testBinPath(t)
	// cmd.Dir 必须设:init 用的是当前目录,漏了它就会在本仓库目录里建出 .minirepo/
	// (真的发生过:于是补全取不到项目、测试红得莫名其妙)
	cmd := exec.Command(bin, "init", "-u", mf, "-m", "default.xml")
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture init: %v\n%s", err, out)
	}
	// 直接取 in-process 的脚本(和 CLI 输出同一函数),避免多一次子进程
	script = filepath.Join(root, "comp.bash")
	if err := os.WriteFile(script, []byte(mustCompletion("bash")), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, script, bin
}

func filepathToSlash(p string) string { return filepath.ToSlash(p) }

func testBinPath(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("MINIREPO_TEST_BIN")
	if bin == "" {
		bin = filepath.Join(os.TempDir(), "minirepo-test-bin")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("test binary missing (%v): TestMain builds it", err)
	}
	return bin
}

func TestCompletionRuntimeBehaviour(t *testing.T) {
	requireBash(t)
	ws, script, bin := compFixture(t)
	// COMP_WORDS[0] 必须是脚本可执行的**路径**:__mr_exe 对含 / 的词直接用,
	// 否则才去 PATH 里找。测试二进制不在 PATH 上。
	prog := bin
	has := func(name string, got []string, want string) {
		t.Helper()
		for _, g := range got {
			if g == want {
				return
			}
		}
		t.Errorf("%s: %q not in %v", name, want, got)
	}
	exact := func(name string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}

	// 1. 子命令前缀
	got := compRun(t, script, bin, []string{prog, "syn"}, ws)
	exact("subcommand prefix", got, "sync")

	// 2. help 的主题是命令名,且不含 help 自身(当初 help 就是这么漏的)
	got = compRun(t, script, bin, []string{prog, "help", "s"}, ws)
	has("help topics", got, "sync")
	has("help topics", got, "status")
	for _, g := range got {
		if g == "help" {
			t.Errorf("help must not complete itself: %v", got)
		}
	}

	// 3. 项目路径:前缀匹配、**不带尾斜杠**(带斜杠会让 sync 报 unknown project)
	got = compRun(t, script, bin, []string{prog, "sync", "Too"}, ws)
	exact("project name must not get a trailing slash", got, "ToolChain")
	got = compRun(t, script, bin, []string{prog, "status", "pkg/"}, ws)
	exact("nested path stays one token", got, "pkg/nested")

	// 4. 上一个词是带值选项时,补它的"值"而不是选项名
	got = compRun(t, script, bin, []string{prog, "complete", "--shell", ""}, ws)
	for _, want := range []string{"bash", "zsh", "fish", "powershell"} {
		has("--shell values", got, want)
	}
	got = compRun(t, script, bin, []string{prog, "sync", "-j", ""}, ws)
	has("-j values", got, "8")
	got = compRun(t, script, bin, []string{prog, "init", "-b", ""}, ws)
	has("-b branches (from the manifests repo)", got, "release/v2")

	// 5. 选项名:每个子命令都得给 -h/--help
	got = compRun(t, script, bin, []string{prog, "sync", "-"}, ws)
	for _, want := range []string{"-h", "--help", "--force", "-j"} {
		has("sync options", got, want)
	}

	// 6. 目录参数(gen 的唯一位置参数)必须补出目录并带尾斜杠
	// gen 的位置参数是目录:在临时根下放一个目录,补全应给出带尾斜杠的候选
	dirArg := filepath.Join(filepath.Dir(ws), "ws")
	got = compRun(t, script, bin, []string{prog, "gen", dirArg}, ws)
	// cur 是完整已存在目录时,bash 会直接接受它;候选本身必须带尾斜杠
	found := false
	for _, g := range got {
		if strings.HasSuffix(g, "/") {
			found = true
		}
	}
	if !found {
		t.Errorf("gen <dir> should offer directories with a trailing slash (cur=%q): %v", dirArg, got)
	}

	// 7. 位置参数不是项目名时(未 init 的目录)不该给出项目候选
	got = compRun(t, script, bin, []string{prog, "nosuchcommand", "x"}, ws)
	if len(got) != 0 {
		t.Errorf("unknown subcommand must yield nothing and let -o default handle it, got %v", got)
	}
}

// TestCompletionSpecUsesDefaultFallback complete -F 会**完全接管**该命令行的补全:
// 少了 -o default,~/xxx<TAB> 就彻底废了(这是最初那批报"目录补不出来"的根因)。
func TestCompletionSpecUsesDefaultFallback(t *testing.T) {
	s := mustCompletion("bash")
	if !strings.Contains(s, "complete -o default -F _minirepo") {
		t.Error("the spec must register with -o default (path fallback)")
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "complete ") && strings.Contains(line, "-o filenames") {
			t.Errorf("-o filenames must not be registered (it appends '/' to project names, and"+
				" 'sync kernel/' is an unknown project): %s", line)
		}
	}
}
