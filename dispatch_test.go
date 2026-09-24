package main

// dispatch_test.go —— 命令分发与退出码。
//
// 这一面以前**一次都没被执行过**(补全测试只查候选,不跑命令),而它恰恰是脚本/CI
// 最先消费的东西:退出码 0/1/2 的分工、usage 走 stderr、`--shell` 每种都得真吐出
// 可加载的脚本。这些错了,报错方式是"CI 里静默成功"或"复制粘贴的补全安装命令什么
// 都没装"。

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAndUsage(t *testing.T) {
	dir := t.TempDir()

	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, out, se := runSplit(t, dir, args...)
		if code != 0 {
			t.Errorf("%v: exit %d (a version query must never fail)", args, code)
		}
		// 恰好一行、恰好一个 token、裸版本串(无程序名前缀):这是给脚本消费的约定,
		// `[ "$(minirepo version)" = v0.10.0 ]` 必须能直接成立。
		if v := strings.TrimSpace(out); v == "" || strings.ContainsAny(v, " \t") || strings.Count(out, "\n") > 1 {
			t.Errorf("%v: version output must be exactly one bare token, got %q", args, out)
		}
		if strings.Contains(se, "error") {
			t.Errorf("%v: noise on stderr: %q", args, se)
		}
	}

	// 裸调用 / 未知命令:usage 走 stderr,退出码 2(与 flag 包的用法错误一致)
	code, out, se := runSplit(t, dir)
	if code != 2 || out != "" {
		t.Errorf("bare invocation: code=%d stdout=%q (want 2 and nothing on stdout)", code, out)
	}
	for _, want := range []string{"Usage:", "sync", "forall"} {
		if !strings.Contains(se, want) {
			t.Errorf("bare invocation must print the command list to stderr (missing %q): %q", want, se)
		}
	}
	code, out, se = runSplit(t, dir, "nosuchcmd")
	if code != 2 || !strings.Contains(se, `"nosuchcmd"`) {
		t.Errorf("unknown command: code=%d stderr=%q (must name what it rejected)", code, se)
	}

	// help:总览 + 单个命令 + 未知主题
	// 裸 help 是一次成功的信息请求:总览进 stdout、exit 0(和"没给命令"区分开)
	code, out, se = runSplit(t, dir, "help")
	if code != 0 || out == "" || !strings.Contains(out, "Usage:") {
		t.Errorf("`help`: code=%d stdout=%q stderr=%q (want 0 + the overview on stdout)", code, out, se)
	}
	if strings.Contains(se, "error") {
		t.Errorf("`help` reported an error: %q", se)
	}
	code, out, _ = runSplit(t, dir, "help", "sync", "list")
	if code != 0 || !strings.Contains(out, "--force") || !strings.Contains(out, "--paths") {
		t.Errorf("`help sync list` must show both commands' real options: code=%d %q", code, out)
	}
	code, _, se = runSplit(t, dir, "help", "nosuch")
	if code != 2 || !strings.Contains(se, "nosuch") {
		t.Errorf("unknown help topic: code=%d stderr=%q (want 2 + the name)", code, se)
	}
}

// 每种 shell 都得吐出**真能加载**的脚本。这里只查结构与专属标记(实机 TAB 由人在
// zsh/fish/powershell 里验);不认识的 shell 必须失败并列出支持的名字。
func TestCompleteEmitsEveryAdvertisedShell(t *testing.T) {
	markers := map[string]string{
		"bash":       "complete -o default -F _minirepo",
		"zsh":        "bashcompinit",
		"fish":       "function __minirepo",
		"powershell": "Register-ArgumentCompleter",
	}
	for sh, marker := range markers {
		code, out, se := runSplit(t, t.TempDir(), "complete", "--shell", sh)
		if code != 0 {
			t.Errorf("complete --shell %s: exit %d (%s)", sh, code, se)
			continue
		}
		if !strings.Contains(out, marker) {
			t.Errorf("complete --shell %s: no %q in the script -- it is not the right dialect", sh, marker)
		}
		// 未替换的模板占位符 = 生成逻辑漏了一步(用户会拿到一堆字面量 SHELLS)
		if strings.Contains(out, "SHELLS") {
			t.Errorf("complete --shell %s: template placeholder left in the output", sh)
		}
	}
	code, _, se := runSplit(t, t.TempDir(), "complete", "--shell", "tcsh")
	if code == 0 || !strings.Contains(se, "bash") || !strings.Contains(se, "zsh") {
		t.Errorf("unsupported shell must fail listing what works: code=%d %q", code, se)
	}
}

// 退出码的分工是契约的一部分:MISSING 才是失败,dirty 不是。
func TestExitCodesDistinguishMissingFromDirty(t *testing.T) {
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master" sync-c="true"/>
  <project name="proj" path="proj"/>
</manifest>`)

	code, out, _ := runSplit(t, ws, "status")
	if code != 1 || !strings.Contains(out, "MISSING") {
		t.Errorf("status before sync must report MISSING and exit 1 (scripts gate on it): code=%d %q", code, out)
	}
	if code, out, se := runSplit(t, ws, "sync"); code != 0 {
		t.Fatalf("sync: %d %s", code, out+se)
	}
	writeFile(t, filepath.Join(ws, "proj", "f"), "local\n")
	code, out, _ = runSplit(t, ws, "status")
	if code != 0 || !strings.Contains(out, "[dirty]") {
		t.Errorf("a dirty project is information, not failure: code=%d %q", code, out)
	}

	// 过滤:未知名字要指名并失败(静默跑 0 个仓是最容易骗过 CI 的错)
	code, _, se := runSplit(t, ws, "sync", "nosuchproject")
	if code == 0 || !strings.Contains(se, "unknown project") || !strings.Contains(se, "nosuchproject") {
		t.Errorf("unknown filter: code=%d stderr=%q", code, se)
	}
	// 尾斜杠容忍(补全给出 `proj/`、手打也是自然写法)。上面刚把 proj 弄脏了,
	// 所以用 --dry-run --force 来验"过滤接受它",而不是被 dirty 拒绝干扰。
	if code, out, se := runSplit(t, ws, "sync", "--dry-run", "--force", "proj/"); code != 0 ||
		!strings.Contains(out, "would fetch+checkout") || strings.Contains(out, "0 ok") {
		t.Errorf("trailing slash in a filter must be tolerated: code=%d %s", code, out+se)
	}
	if code, out, se := runSplit(t, ws, "list", "proj/"); code != 0 || !strings.Contains(out, "proj") {
		t.Errorf("list must accept the same filter form: code=%d %s", code, out+se)
	}
}

// `list --paths` 是补全与脚本的输入:一行一个、无警告、无 ANSI、无表头。
func TestListPathsIsMachineReadable(t *testing.T) {
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
  <project name="second" path="deep/second"/>
</manifest>`)
	code, out, _ := runSplit(t, ws, "list", "--paths")
	if code != 0 {
		t.Fatalf("list --paths: exit %d", code)
	}
	got := strings.Fields(strings.ReplaceAll(out, "\n", " "))
	want := []string{"deep/second", "proj"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("list --paths = %v, want %v (sorted, slash form)", got, want)
	}
	if strings.Contains(out, "\033[") || strings.Contains(out, "warning") {
		t.Errorf("machine-readable stream polluted: %q", out)
	}
}
