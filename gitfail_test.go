package main

// gitfail_test.go —— git 子进程失败时的**输出契约**与环境准备。
//
// 触发方式是真实的:`kill -9 minirepo` 会留下活的 git 子进程和它没释放的
// `.git/index.lock`,于是之后每一次 sync 都撞上同一把锁。这不是构造出来的场景,
// 是每天会发生的事,而用户看到的应该是"哪把锁、怎么办",不是一段读不通的诗。

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const lockManifest = `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
</manifest>`

func TestStaleGitLockKeepsTheReportContract(t *testing.T) {
	_, ws, _ := gitManifestWS(t, lockManifest)
	if code, _, se := runSplit(t, ws, "sync"); code != 0 {
		t.Fatalf("baseline sync: %s", se)
	}
	lock := filepath.Join(ws, "proj", ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, se := runSplit(t, ws, "sync")
	if code == 0 {
		t.Fatal("sync succeeded while .git/index.lock exists -- git refused, we must report that")
	}
	lines := nonEmpty(out)
	// 一项目 + 一行汇总。以前这里是 4 行(git 的续行被塞进报告行),而且第 2~4 行
	// 开头没有 [xx] 标记,长得像我们自己的输出。
	if len(lines) != 2 {
		t.Fatalf("stdout must stay one line per project + summary, got %d lines:\n%s", len(lines), out)
	}
	for _, ln := range lines {
		if strings.Contains(ln, "\n") {
			t.Errorf("report line contains a newline: %q", ln)
		}
	}
	if !strings.Contains(lines[0], "[FAIL]") || !strings.Contains(lines[0], "proj") {
		t.Errorf("first line must be the project's own verdict, got %q", lines[0])
	}
	// 重点必须在:是哪把锁(而不是 git 那段话的最后半句)。
	if !strings.Contains(lines[0], "index.lock") {
		t.Errorf("the report line must name the blocking file, got %q", lines[0])
	}
	if strings.Contains(lines[0], "are terminated then try again") {
		t.Error("we are again surfacing git's continuation text as our message")
	}
	// 锁必须**还在**:它可能属于一个还活着的 git 子进程(kill -9 只杀父进程),自动删
	// 掉一把活锁就是在损坏仓库。我们只负责把它指出来。
	if _, err := os.Stat(lock); err != nil {
		t.Error("the failed sync removed .git/index.lock -- refusing is the whole answer, deleting is not")
	}
	// 完整原文不能丢:走 stderr,带归属前缀。期望值不硬编码 git 的措辞——同一句
	// stale-lock 提示在 git 2.43 是 8 行长版(含 remove the file manually),新版是
	// 3 行短版,耦合措辞必然随 git 升级假红(CI 实测)。以**本机 git 的原话**为基准:
	// 同一把锁、同一条 argv 直跑一遍取原始输出,逐行核对带前缀到达,任何版本都成立。
	if !strings.Contains(se, "  git: fatal:") {
		t.Errorf("fatal line must reach stderr with attribution, got %q", se)
	}
	rawCmd := exec.Command("git", "-C", filepath.Join(ws, "proj"),
		"checkout", "-f", "--detach", "origin/master")
	raw, _ := rawCmd.CombinedOutput()
	if lines := nonEmpty(string(raw)); len(lines) == 0 {
		t.Fatalf("premise broken: git must refuse on a stale index.lock, got %q", raw)
	} else {
		for _, ln := range lines {
			if !strings.Contains(se, "  git: "+ln) {
				t.Errorf("git's own words must reach stderr with attribution; line %q missing, stderr=%q", ln, se)
			}
		}
	}

	// 只读命令在锁存在时必须照常可用(有意的决定:清单坏了不该让 status 也不能用)
	if code, out, se := runSplit(t, ws, "status"); code != 0 || !strings.Contains(out, "proj") {
		t.Errorf("status must stay usable with a stale lock: code=%d %s%s", code, out, se)
	}

	// 用户按提示把锁删掉之后,必须能正常恢复
	os.Remove(lock)
	if code, out, se := runSplit(t, ws, "sync"); code != 0 {
		t.Errorf("sync must recover once the lock is gone: code=%d %s%s", code, out, se)
	}
}

// TestWeNeverDeleteTheLockForThem 钉住一个"不做什么"的决定:锁可能是**活的** git
// 留下的(`kill -9 minirepo` 只杀父进程,git 子进程还在写)。自动 rm 掉一把活锁就是
// 损坏仓库,所以只能拒绝 + 说清是哪把锁。
func TestWeNeverDeleteTheLockForThem(t *testing.T) {
	for _, f := range []string{"sync.go", "workspace.go", "commands.go", "archive.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, ln := range strings.Split(string(src), "\n") {
			if strings.Contains(ln, "index.lock") && !strings.Contains(strings.TrimSpace(ln), "//") {
				t.Errorf("%s touches index.lock in code (%q): refusing must be the whole answer", f, ln)
			}
		}
	}
}

func TestGitVerdictPicksTheConclusion(t *testing.T) {
	for _, tc := range []struct{ name, out, want string }{
		{"fatal first then continuation",
			"fatal: Unable to create '/w/p/.git/index.lock': File exists.\n\nAnother git process seems to be running\nremove the file manually to continue.",
			"fatal: Unable to create '/w/p/.git/index.lock': File exists."},
		{"progress noise then the real error",
			"remote: Enumerating objects: 12\nremote: Counting objects: 100%\nfatal: could not read Username for 'https://host'",
			"fatal: could not read Username for 'https://host'"},
		{"no prefix at all", "not a git repository", "not a git repository"},
		{"blank", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitVerdict(tc.out); got != tc.want {
				t.Errorf("gitVerdict(%q) = %q, want %q", briefOut(tc.out), got, tc.want)
			}
			if strings.Contains(gitVerdict(tc.out), "\n") {
				t.Error("verdict must be a single line")
			}
		})
	}
	// 报告行的形状:一行"动词: 结论"(原文由调用点写到 stderr)
	msg := gitFailure("checkout failed", "error: Your local changes\nwould be overwritten\n\ndied of exhaustion")
	if strings.Contains(msg, "\n") {
		t.Errorf("gitFailure must return one line: %q", msg)
	}
	if !strings.HasPrefix(msg, "checkout failed: error: Your local changes") {
		t.Errorf("verb + verdict expected, got %q", msg)
	}
}

// TestCapturedGitNeverAsksForCredentials 钉住 gitEnv 的三分法。凭据提示是 git 直接
// 开 /dev/tty 写的,不受 stdin 重定向影响 —— 所以"能不能问人"必须显式决定,而探测类
// 调用一次都不该问(否则 `minirepo status` 能挂到 gitTimeout 才失败)。
func TestCapturedGitNeverAsksForCredentials(t *testing.T) {
	const noPrompt = "GIT_TERMINAL_PROMPT=0"
	origTTY := isTTY
	t.Cleanup(func() { isTTY = origTTY })

	prompts := func(env []string) bool { return !slices.Contains(env, noPrompt) }

	isTTY = func() bool { return true }
	if !prompts(gitEnv(true)) {
		t.Error("interactive git at a terminal must still be able to ask for credentials (capability loss)")
	}
	if prompts(gitEnv(false)) {
		t.Error("captured probes must never ask: nobody is reading the answer")
	}
	isTTY = func() bool { return false }
	if prompts(gitEnv(true)) {
		t.Error("with output redirected (CI, log, pipe) even clone/fetch must fail fast instead of hanging")
	}
	// 继承真实环境:漏掉 os.Environ() 会让 git 找不到 PATH/HOME/凭据助手
	if env := gitEnv(false); !containsAny(env, "PATH=") {
		t.Errorf("child env lost the parent's environment: %d vars", len(env))
	}
}

func containsAny(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func nonEmpty(s string) []string {
	out := []string{}
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

func briefOut(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
