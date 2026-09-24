package main

// main_test.go —— 契约回归测试。
// 规则来源:CONTRACT.md。这里钉住的是
// 那些曾经静默出错的语义:相对 fetch 锚定、url= 覆盖、remove-project 文档顺序、
// 缺 revision 不猜、工作区边界、传输白名单、分离检出跟随远端 tip、
// dirty 拒绝、dry-run 如实、嵌套项目按深度分波、freeze 往返。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- 基础设施 ----------

// mustDie 断言 fn 里的 die() 被触发且消息包含 substr
func mustDie(t *testing.T, substr string, fn func()) {
	t.Helper()
	orig := die
	defer func() { die = orig }()
	die = func(f string, a ...any) { panic(fmt.Sprintf(f, a...)) }
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected die(%q...), nothing panicked", substr)
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, substr) {
			t.Fatalf("die message %q does not contain %q", msg, substr)
		}
	}()
	fn()
}

// guard 把命令层的退出改成 panic:cmdSync/cmdStatus 里任何 exit 都是失败信号,
// 但直接 os.Exit 会连测试进程一起杀掉(看不到是哪个断言挂了)
func guard(t *testing.T) {
	t.Helper()
	orig := exitNow
	exitNow = func(code int) { panic(fmt.Sprintf("unexpected exit(%d)", code)) }
	t.Cleanup(func() { exitNow = orig })
}

// g 在 dir 里跑 git(自动带身份);失败 fatal
func g(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=t@e", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// mkRepo 建一个带两个跟踪文件的仓(我的改动与上游改动永不冲突)
func mkRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	g(t, dir, "init", "-q", "--initial-branch=master")
	os.WriteFile(filepath.Join(dir, "f"), []byte("v1\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "g"), []byte("g1\n"), 0o644)
	g(t, dir, "add", "-A")
	g(t, dir, "commit", "-q", "-m", "seed")
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------- 解析层表驱动 ----------

func TestAbsolutize(t *testing.T) {
	cases := []struct{ base, url, want string }{
		{"https://h/m/manifests.git", "../kernel", "https://h/m/kernel"},
		{"https://h/m/manifests.git", "../../pub/x", "https://h/pub/x"},
		{"https://h/mf", "sub/r.git", "https://h/mf/sub/r.git"},
		{"https://h/mf", "https://other/x", "https://other/x"}, // 绝对不锚定
		{"https://h/mf", "git@h:p/r.git", "git@h:p/r.git"},     // scp 形态
		{"https://h/mf", "/local/r", "/local/r"},               // 绝对路径
		{"https://h/mf", "C:\\r", "C:\\r"},                     // Windows 盘符
		{"file:///tmp/mf", "../src/r", "file:///tmp/src/r"},
	}
	for _, c := range cases {
		if got := absolutize(c.base, c.url); got != c.want {
			t.Errorf("absolutize(%q, %q) = %q, want %q", c.base, c.url, got, c.want)
		}
	}
}

func TestAbsolutizeErrors(t *testing.T) {
	mustDie(t, "relative URL", func() { absolutize("", "../x") })
	mustDie(t, "relative", func() { absolutize("../base", "../x") })
}

func TestWithin(t *testing.T) {
	ok := []struct{ in, want string }{
		{"kernel", "kernel"}, {"a/b/", "a/b"}, {".\\a\\b", "a/b"},
		{"./a", "a"}, {"a/./b", "a/b"},
	}
	for _, c := range ok {
		if got := within(c.in, "<p>"); got != c.want {
			t.Errorf("within(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"../escape", "/abs", "a/../..", ".", "C:\\x"} {
		mustDie(t, "inside the workspace", func() { within(bad, "<p>") })
	}
}

func TestSafeURL(t *testing.T) {
	for _, u := range []string{"https://h/r", "http://h/r", "git://h/r", "ssh://h/r",
		"file:///tmp/r", "git@host:team/r.git", "/local/r", "C:\\r"} {
		if got := safeURL(u, "t"); got != u {
			t.Errorf("safeURL(%q) changed to %q", u, got)
		}
	}
	mustDie(t, "ext::", func() { safeURL("ext::sh -c id", "t") })
	mustDie(t, "transport", func() { safeURL("fd::0", "t") })
	mustDie(t, "not a usable URL", func() { safeURL("-oops", "t") })
	mustDie(t, "not a usable URL", func() { safeURL("", "t") })
	mustDie(t, "unsupported transport", func() { safeURL("ldap://h/r", "t") })
}

func TestMaskURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://user:secret@host/team/r.git", "https://***@host/team/r.git"},
		{"http://u@host/r", "http://***@host/r"},
		{"https://host/r", "https://host/r"},
		{"git@host:team/r.git", "git@host:team/r.git"}, // scp 形态无 scheme,不在掩盖范围
	}
	for _, c := range cases {
		if got := maskURL(c.in); got != c.want {
			t.Errorf("maskURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------- 清单解析 ----------

const headerXML = `<manifest>
  <remote name="o" fetch="%s"/>
  <default remote="o" revision="master"/>
  <project name="keep" path="keep"/>
</manifest>`

func TestParseManifestSubset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "m.xml"),
		`<manifest>
  <remote name="o" fetch="../src"/>
  <remote name="b" fetch="https://backup/"/>
  <default remote="o" revision="master" sync-c="true"/>
  <project name="keep" path="keep"/>
  <project name="ov" path="ov" url="https://direct/r.git"/>
  <project name="link" path="linked"><copyfile src="f" dest="link.mk"/></project>
  <linkfile src="x" dest="y"/>
  <project name="gone" path="gone"/>
  <remove-project name="gone"/>
  <remove-project name="keep"/>
  <project name="keep" path="readded"/>
  <default revision="dev" remote="b"/>
</manifest>`)

	m := parseManifestFile(dir, "m.xml")

	if len(m.Ignored) != 1 || !m.Ignored["<linkfile>"] {
		t.Errorf("ignored = %v, want only <linkfile>", m.Ignored)
	}
	// 同名取最后定义:keep 被裁后又在 remove 之后重新定义 → 存活且 path=readded
	last := map[string]RawProject{}
	for _, p := range m.Projects {
		last[p.Name] = p
	}
	if p, ok := last["keep"]; !ok || p.Dead || p.Path != "readded" {
		t.Errorf("keep = %+v (dead=%v), want readded alive (remove-then-readd)", p, p.Dead)
	}
	if p, ok := last["gone"]; ok && !p.Dead {
		t.Error("gone should be removed")
	}
	// include 层面的父优先在 TestParseManifestInclude 里测
	_ = last
	// default:文档顺序后面的 <default> 覆盖前面的
	if m.Default.Remote != "b" || m.Default.Revision != "dev" || !m.Default.syncCSeen || !m.Default.SyncC {
		t.Errorf("default = %+v", m.Default)
	}
	if last["ov"].URL != "https://direct/r.git" {
		t.Errorf("url= override lost: %q", last["ov"].URL)
	}
	if len(last["link"].Copyfiles) != 1 {
		t.Errorf("copyfile not collected: %+v", last["link"])
	}
}

func TestParseManifestIncludeAndSyncC(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "base.xml"), `<manifest>
  <remote name="o" fetch="../src"/>
  <remote name="extra" fetch="https://extra/"/>
  <default remote="o" revision="master" sync-c="true"/>
  <project name="frombase" path="fb"/>
</manifest>`)
	writeFile(t, filepath.Join(dir, "top.xml"), `<manifest>
  <include name="base.xml"/>
  <remote name="extra" fetch="https://parent-wins/"/>
  <default revision="dev"/>
  <project name="fromtop" path="ft"/>
</manifest>`)

	m := parseManifestFile(dir, "top.xml")
	if m.Remotes["extra"] != "https://parent-wins/" {
		t.Errorf("include merge: parent must win, got %q", m.Remotes["extra"])
	}
	if m.Default.Revision != "dev" || !m.Default.SyncC {
		t.Errorf("default merge wrong: %+v (sub 的 sync-c=true 应被继承)", m.Default)
	}
	names := map[string]bool{}
	for _, p := range m.Projects {
		names[p.Name] = true
	}
	if !names["frombase"] || !names["fromtop"] {
		t.Errorf("projects = %v (两个来源的项目都必须在场)", names)
	}

	// 父文件显式 sync-c=false 时,子清单的 true 不得覆盖
	writeFile(t, filepath.Join(dir, "off.xml"), `<manifest>
  <include name="base.xml"/>
  <default revision="master" sync-c="false"/>
</manifest>`)
	m2 := parseManifestFile(dir, "off.xml")
	if m2.Default.SyncC {
		t.Error("explicit sync-c=false must not be overridden by included true")
	}
}

func TestResolveRequiresRevision(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "norev.xml"), `<manifest>
  <remote name="o" fetch="../src"/>
  <default remote="o"/>
  <project name="keep" path="keep"/>
</manifest>`)
	m := parseManifestFile(dir, "norev.xml")
	mustDie(t, "no revision", func() { resolveProjects(m, dir) })
}

func TestResolveRelativeFetchIsAnchored(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	writeFile(t, filepath.Join(src, "f"), "x")
	g(t, src, "init", "-q", "--initial-branch=master")
	g(t, src, "add", "-A")
	g(t, src, "commit", "-q", "-m", "x")
	writeFile(t, filepath.Join(dir, "rel.xml"),
		fmt.Sprintf(`<manifest>
  <remote name="o" fetch="../src/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
</manifest>`))
	m := parseManifestFile(dir, "rel.xml")
	ps := resolveProjects(m, "https://example.com/team/manifests.git")
	if len(ps) != 1 {
		t.Fatal("no project resolved")
	}
	if want := "https://example.com/team/src/proj"; ps[0].URL != want {
		t.Errorf("url = %q, want %q (.. 去掉一段)", ps[0].URL, want)
	}
}

func TestResolveBoundary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "esc.xml"), `<manifest>
  <remote name="o" fetch="/src"/>
  <default remote="o" revision="master"/>
  <project name="p" path="../escape"/>
</manifest>`)
	m := parseManifestFile(dir, "esc.xml")
	mustDie(t, "inside the workspace", func() { resolveProjects(m, "") })
}

func TestFilterProjects(t *testing.T) {
	ps := []Project{{Name: "r", Path: "prebuilt/toolchain"}}
	as := []Archive{{Path: "dl"}}
	got, _ := filterProjects(ps, as, []string{"prebuilt/toolchain/"})
	if len(got) != 1 {
		t.Error("trailing slash must be tolerated")
	}
	mustDie(t, "unknown project", func() { filterProjects(ps, as, []string{"nosuch"}) })
}

// ---------- 端到端 ----------

func TestSyncE2E(t *testing.T) {
	root := t.TempDir()
	// repo 的 URL 规则是 fetch+name 直拼,所以 fetch 是前缀目录 up/,仓在 up/proj
	up := filepath.Join(root, "up")
	mf := filepath.Join(root, "mf")
	ws := filepath.Join(root, "ws")
	os.MkdirAll(mf, 0o755)
	os.MkdirAll(ws, 0o755)
	mkRepo(t, filepath.Join(up, "proj"))
	mkRepo(t, filepath.Join(up, "nested"))
	writeFile(t, filepath.Join(mf, "default.xml"), fmt.Sprintf(`<manifest>
  <remote name="o" fetch="%s/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
  <project name="nested" path="proj/nested"/>
</manifest>`, filepath.ToSlash(up)))
	g(t, mf, "init", "-q", "--initial-branch=master")
	g(t, mf, "add", "-A")
	g(t, mf, "commit", "-q", "-m", "manifest")

	// init → sync
	chdir(t, ws)
	cmdInit([]string{"-u", mf, "-m", "default.xml"})
	cmdSync(nil)
	// 进程内的 cmd* 一旦走退出路径就是 bug,让它 panic 出来而不是杀掉测试进程
	guard(t)
	head := func() string { return g(t, filepath.Join(ws, "proj"), "rev-parse", "HEAD") }
	seed := head()

	// 嵌套项目也被检出(深度分波:父先完成)
	if !fileExists(filepath.Join(ws, "proj", "nested", ".git")) {
		t.Fatal("nested project not checked out")
	}

	// 上游前进 + 我本地干净 → sync 必须跟上(分离检出跟随远端 tip)
	g(t, filepath.Join(up, "proj"), "commit", "-q", "--allow-empty", "-m", "second")
	cmdSync(nil)
	if head() == seed {
		t.Fatal("sync did not advance to the fetched tip (0.5.2 bug regressed)")
	}

	// 我本地有未提交改动 → 拒绝,且改动完好
	os.WriteFile(filepath.Join(ws, "proj", "f"), []byte("local edit\n"), 0o644)
	// 用清单解析出来的真实项目(测试不复述解析逻辑,也不手拼 URL)
	_, ps, _ := resolved(ws, false)
	proj := ps[0]
	if proj.Path != "proj" {
		t.Fatalf("expected proj first, got %s", proj.Path)
	}
	r := syncOne(ws, proj, false, false, false)
	if r.ok || !strings.Contains(r.msg, "local changes") {
		t.Fatalf("dirty sync must refuse: %+v", r)
	}
	if st, _ := os.ReadFile(filepath.Join(ws, "proj", "f")); string(st) != "local edit\n" {
		t.Fatal("local changes were touched by a refused sync")
	}

	// dry-run 如实:报 would refuse(真实行为),不撒谎
	rc, out := runForOutput(ws, "sync", "--dry-run")
	if rc != 1 || !strings.Contains(out, "would refuse") {
		t.Fatalf("dry-run must mirror the refusal: rc=%d out=%s", rc, out)
	}

	// --force 翻转判定且不动文件
	rr := syncOne(ws, proj, true, false, true)
	if !rr.ok || !strings.Contains(rr.msg, "would fetch+checkout") {
		t.Fatalf("force dry-run verdict wrong: %+v", rr)
	}

	// freeze → 新工作区用快照重建 → HEAD 一致
	snap := filepath.Join(mf, "snap.xml")
	cmdFreeze([]string{"-o", snap})
	g(t, mf, "add", "-A")
	g(t, mf, "commit", "-q", "-m", "snapshot")
	ws2 := filepath.Join(root, "ws2")
	os.MkdirAll(ws2, 0o755)
	chdir(t, ws2)
	cmdInit([]string{"-u", mf, "-m", "snap.xml"})
	cmdSync(nil)
	if h := g(t, filepath.Join(ws2, "proj"), "rev-parse", "HEAD"); h != head() {
		t.Fatalf("snapshot rebuild HEAD mismatch: %s != %s", h, head())
	}
}

// runForOutput 以子进程跑本工具(需要独立进程才能断言 os.Exit 的码)
func runForOutput(dir string, args ...string) (int, string) {
	bin := os.Getenv("MINIREPO_TEST_BIN")
	if bin == "" {
		bin = filepath.Join(os.TempDir(), "minirepo-test-bin")
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), string(out)
		}
		return 127, string(out)
	}
	return 0, string(out)
}

func TestMain(m *testing.M) {
	// 为子进程断言(exit code)准备一个当前源码的二进制
	bin := filepath.Join(os.TempDir(), "minirepo-test-bin")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "test setup: build failed: %v\n%s", err, out)
		os.Exit(1)
	}
	defer os.Remove(bin)
	os.Exit(m.Run())
}
