package main

// compat_test.go —— 已经实现、但之前**一次都没被测过**的行为。
// 每一条都对应一个"错了也不会立刻发现"的类别:凭据外泄、静默丢改动、
// 预览撒谎、输出格式(脚本消费方依赖它)。全部走真 CLI(子进程),因为
// "stdout 是否干净"这类事实在进程内测不到。

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSplit 分开收 stdout / stderr:契约要求 stdout 机器可读(警告只能进 stderr),
// 合成一个流就永远测不出泄漏。
func runSplit(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(testBinPath(t), args...)
	cmd.Dir = dir
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return code, so.String(), se.String()
}

// gitManifestWS 建一个 上游仓 + 清单仓 + 已 init 工作区,返回工作区路径。
func gitManifestWS(t *testing.T, manifest string) (root, ws, mfDir string) {
	t.Helper()
	root = t.TempDir()
	up := filepath.Join(root, "up")
	mkRepo(t, filepath.Join(up, "proj"))
	mfDir = filepath.Join(root, "mf")
	os.MkdirAll(mfDir, 0o755)
	writeFile(t, filepath.Join(mfDir, "default.xml"), manifest)
	g(t, mfDir, "init", "-q", "--initial-branch=master")
	g(t, mfDir, "add", "-A")
	g(t, mfDir, "commit", "-q", "-m", "manifest")
	ws = filepath.Join(root, "ws")
	os.MkdirAll(ws, 0o755)
	if code, _, se := runSplit(t, ws, "init", "-u", mfDir, "-m", "default.xml"); code != 0 {
		t.Fatalf("init: %s", se)
	}
	return root, ws, mfDir
}

// 1. 凭据只能以 *** 出现。相对 fetch 会把清单仓 URL 里的凭据带进**每一条**
// project/archive URL,所以顶层 mask 过了还不够(那是 py 抓过的同一个洞)。
func TestCredentialsNeverDisplayed(t *testing.T) {
	const secret = "s3cret-token"
	up, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
</manifest>`)

	// 把清单仓 URL 换成"带凭据的远端"(值本身不存在,只用于解析与显示):
	// 真实场景是 init -u https://user:token@host/mf.git
	cfgPath := filepath.Join(ws, toolDir, configFile)
	cfg := loadConfigFile(cfgPath)
	if cfg == nil {
		t.Fatal("no config after init")
	}
	cfg.URL = "https://ci-bot:" + secret + "@192.0.2.1/pub/mf.git"
	saveConfig(ws, cfg)

	code, out, se := runSplit(t, ws, "manifest")
	if code != 0 {
		t.Fatalf("manifest: %s", se)
	}
	if strings.Contains(out, secret) || strings.Contains(se, secret) {
		t.Errorf("`manifest` printed the credential")
	}
	// 掩盖整个 userinfo(不保留用户名):`https://<token>@host` 这种把令牌放在
	// username 位置的写法很常见,保留 username 就等于可能泄了令牌。
	if !strings.Contains(out, "://***@") {
		t.Errorf("`manifest` shows no masked URL at all -- the assertion above would pass vacuously (out=%q)", out)
	}

	code, out, se = runSplit(t, ws, "manifest", "--json")
	if code != 0 {
		t.Fatalf("manifest --json: %s", se)
	}
	var parsed manifestJSON
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("`manifest --json` stdout is not valid JSON (warnings must go to stderr): %v\n%s", err, out)
	}
	if strings.Contains(out, secret) {
		t.Error("`manifest --json` leaked the credential into machine-readable output")
	}
	if len(parsed.Projects) != 1 || !strings.Contains(parsed.Projects[0].URL, "://***@") {
		t.Errorf("per-project URL not masked (relative fetch embeds the creds): %+v", parsed.Projects)
	}

	code, out, _ = runSplit(t, ws, "list", "-a")
	if code != 0 || strings.Contains(out, secret) {
		t.Errorf("`list -a` failed or leaked: code=%d %q", code, out)
	}
	_ = up
}

// 2. --update-manifests 会 reset 清单检出:丢人工作之前必须喊一声。
func TestUpdateManifestsWarnsBeforeDiscarding(t *testing.T) {
	_, ws, mfDir := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
</manifest>`)
	if code, _, se := runSplit(t, ws, "sync"); code != 0 {
		t.Fatalf("baseline sync: %s", se)
	}
	// 有人在清单检出里手改调试
	dirty := filepath.Join(ws, toolDir, manifestsDir, "default.xml")
	b, _ := os.ReadFile(dirty)
	writeFile(t, dirty, strings.Replace(string(b), "<project", "<!-- edited by hand -->\n  <project", 1))
	// 清单仓上游又有新提交(触发 reset+pull)
	writeFile(t, filepath.Join(mfDir, "extra.xml"), "<manifest><remote name=\"o\" fetch=\"../up/\"/><default remote=\"o\" revision=\"master\"/></manifest>")
	g(t, mfDir, "add", "-A")
	g(t, mfDir, "commit", "-q", "-m", "extra")

	code, out, se := runSplit(t, ws, "sync", "--update-manifests")
	if code != 0 {
		t.Fatalf("sync --update-manifests: %s", se)
	}
	if !strings.Contains(se, "discarding local edits") {
		t.Errorf("destroying someone's edits without a word: stderr=%q", se)
	}
	after, _ := os.ReadFile(dirty)
	if strings.Contains(string(after), "edited by hand") {
		t.Error("the warning said it resets, but the edit is still there (contract broken the other way)")
	}
	if !strings.Contains(out, "1 ok, 0 failed") {
		t.Errorf("unexpected sync summary: %q", out)
	}
}

// 3. 一个路径一个主人。三类重复都要拒绝,而且报错要点名双方(归档之间冲突时
// 原先会把对方说成 "project <路径>")。
func TestPathConflictsNameBothOwners(t *testing.T) {
	cases := []struct {
		name, file, manifest, want string
	}{
		{"two projects", "dup.xml", `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="a" path="same"/>
  <project name="b" path="same"/>
</manifest>`, `path "same" is claimed by both project a and project b`},
		{"project and archive", "mix.xml", `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="a" path="same"/>
  <archive url="../up/b" path="same"/>
</manifest>`, `path "same" is claimed by both project a and archive`},
		{"two archives", "arch.xml", `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <archive url="../up/a" path="same"/>
  <archive url="../up/b" path="same"/>
</manifest>`, `claimed by both archive `},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			up := filepath.Join(root, "up")
			mkRepo(t, filepath.Join(up, "a"))
			mkRepo(t, filepath.Join(up, "b"))
			mf := filepath.Join(root, "mf")
			os.MkdirAll(mf, 0o755)
			writeFile(t, filepath.Join(mf, tc.file), tc.manifest)
			writeFile(t, filepath.Join(mf, "default.xml"), tc.manifest)
			g(t, mf, "init", "-q", "--initial-branch=master")
			g(t, mf, "add", "-A")
			g(t, mf, "commit", "-q", "-m", "m")
			ws := filepath.Join(root, "ws")
			os.MkdirAll(ws, 0o755)
			if code, _, _ := runSplit(t, ws, "init", "-u", mf, "-m", "default.xml"); code != 0 {
				t.Fatal("init failed")
			}
			// --dry-run 也要拦:预览阶段就该看见,而不是等真下载后互相覆盖
			code, out, se := runSplit(t, ws, "sync", "--dry-run")
			if code == 0 || !strings.Contains(se, tc.want) {
				t.Errorf("want refusal %q, got code=%d out=%q stderr=%q", tc.want, code, out, se)
			}
		})
	}
}

// 4. freeze 的产物必须能被"另一个工作区"直接用,并且 status 全 clean。
// 否则它只是个好看的文本文件 —— 快照的价值恰恰在可复现。
func TestFreezeSnapshotRebuildsAWorkspace(t *testing.T) {
	root, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
</manifest>`)
	if code, _, se := runSplit(t, ws, "sync"); code != 0 {
		t.Fatalf("baseline sync: %s", se)
	}
	snap := filepath.Join(root, "snap.xml")
	if code, _, se := runSplit(t, ws, "freeze", "-o", snap); code != 0 {
		t.Fatalf("freeze: %s", se)
	}
	body, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "s3cret") {
		t.Error("freeze must write real URLs (a snapshot with masked URLs is unusable)")
	}
	// 用快照当清单,在全新工作区重建
	mf2 := filepath.Join(root, "mf2")
	os.MkdirAll(mf2, 0o755)
	writeFile(t, filepath.Join(mf2, "default.xml"), string(body))
	g(t, mf2, "init", "-q", "--initial-branch=master")
	g(t, mf2, "add", "-A")
	g(t, mf2, "commit", "-q", "-m", "snapshot")
	ws2 := filepath.Join(root, "ws2")
	os.MkdirAll(ws2, 0o755)
	if code, _, se := runSplit(t, ws2, "init", "-u", mf2, "-m", "default.xml"); code != 0 {
		t.Fatalf("init from snapshot: %s", se)
	}
	if code, out, se := runSplit(t, ws2, "sync"); code != 0 {
		t.Fatalf("sync from snapshot: %s\n%s", se, out)
	}
	code, out, se := runSplit(t, ws2, "status")
	if code != 0 || !strings.Contains(out, "[clean]") || strings.Contains(out, "dirty") {
		t.Errorf("workspace rebuilt from a snapshot must be clean: code=%d out=%q stderr=%q", code, out, se)
	}
}

// 5. 预览不许撒谎:dry-run 要如实算出真实会做什么/会不会被拒,且不碰磁盘。
func TestDryRunMirrorsRealDecisions(t *testing.T) {
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master" sync-c="true"/>
  <project name="proj" path="proj">
    <copyfile src="f" dest="copied-f"/>
  </project>
</manifest>`)
	// (a) 全新工作区:报 will clone,并把 copyfile 也算进去(它确实会往工作区写文件)
	code, out, _ := runSplit(t, ws, "sync", "--dry-run")
	if code != 0 || !strings.Contains(out, "would clone (+1 copyfile)") {
		t.Errorf("dry-run on a fresh workspace: code=%d out=%q", code, out)
	}
	if fileExists(filepath.Join(ws, "copied-f")) || fileExists(filepath.Join(ws, "proj")) {
		t.Fatal("--dry-run touched the disk")
	}
	if code, out, _ := runSplit(t, ws, "sync"); code != 0 {
		t.Fatalf("real sync: %s", out)
	}
	// (b) 有未提交改动:真实 sync 会拒绝,dry-run 必须同样报拒绝(而不是 would fetch+checkout)
	writeFile(t, filepath.Join(ws, "proj", "f"), "local edit\n")
	code, out, _ = runSplit(t, ws, "sync", "--dry-run")
	if code == 0 || !strings.Contains(out, "would refuse: local changes") {
		t.Errorf("dry-run lied about a dirty project: code=%d out=%q", code, out)
	}
	if code, out, _ := runSplit(t, ws, "sync"); code == 0 || !strings.Contains(out, "local changes") {
		t.Errorf("the real sync must actually refuse: code=%d out=%q", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "proj", "f")); string(b) != "local edit\n" {
		t.Error("refused sync overwrote the local edit anyway")
	}
	// (c) --force 时预览和真实行为一致(都放行)
	if code, out, _ := runSplit(t, ws, "sync", "--dry-run", "--force"); code != 0 ||
		!strings.Contains(out, "would fetch+checkout") {
		t.Errorf("--force dry-run: code=%d out=%q", code, out)
	}
}

// 6. forall 的输出契约:成功只吐命令输出(stdout 可被管道消费),失败才带项目名,
// 并且不假装提供 repo 的 $REPO_* 环境。
func TestForallOutputContract(t *testing.T) {
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <project name="proj" path="proj"/>
</manifest>`)
	if code, _, se := runSplit(t, ws, "sync"); code != 0 {
		t.Fatalf("baseline sync: %s", se)
	}
	code, out, se := runSplit(t, ws, "forall", "-c", "pwd")
	if code != 0 {
		t.Fatalf("forall: %s", se)
	}
	if strings.Contains(out, "===") || strings.Contains(se, "===") {
		t.Errorf("successful forall must not print project headers (repo needs -p for that): out=%q stderr=%q", out, se)
	}
	if !strings.Contains(out, filepath.Join("proj")) {
		t.Errorf("command output missing: %q", out)
	}

	code, out, se = runSplit(t, ws, "forall", "-c", "exit 3")
	if code == 0 {
		t.Error("a failing command must make forall fail (CI relies on it)")
	}
	if !strings.Contains(se, "=== proj ===") {
		t.Errorf("failure must be labelled with the project on stderr: %q", se)
	}
	if strings.Contains(out, "===") {
		t.Errorf("labels belong on stderr, not stdout: %q", out)
	}

	code, _, se = runSplit(t, ws, "forall", "-c", "echo $REPO_PATH")
	if code != 0 {
		t.Fatalf("forall: %s", se)
	}
	if !strings.Contains(se, "$REPO_") {
		t.Errorf("a repo script silently sees empty $REPO_*; say so once: %q", se)
	}
}

//  7. 坏清单必须"干净地失败"。清单是不可信输入,而它由人来编辑:
//     递归没有终点 -> goroutine stack overflow;XML 语法错 -> panic + traceback。
//     两种都不是用户能照着修的东西。
func TestBrokenManifestsFailCleanly(t *testing.T) {
	cases := []struct {
		name, want string
		files      map[string]string
	}{
		{"circular include", "circular <include>", map[string]string{
			"default.xml": `<manifest><include name="a.xml"/></manifest>`,
			"a.xml":       `<manifest><include name="b.xml"/></manifest>`,
			"b.xml":       `<manifest><include name="a.xml"/></manifest>`,
		}},
		{"self include", "circular <include>", map[string]string{
			"default.xml": `<manifest><include name="default.xml"/></manifest>`,
		}},
		{"wrong root element", "not a repo manifest", map[string]string{
			"default.xml": `<projects>
  <project name="a" path="a" revision="master"/>
</projects>`,
		}},
		{"malformed XML", "malformed manifest", map[string]string{
			"default.xml": "<manifest>\n  <project name=\"a\" path=\"a\">\n",
		}},
		{"not XML at all", "not a repo manifest", map[string]string{
			"default.xml": "this is not a manifest\n",
		}},
		{"missing file", "not found", map[string]string{
			"default.xml": `<manifest><include name="nope.xml"/></manifest>`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mf := filepath.Join(root, "mf")
			os.MkdirAll(mf, 0o755)
			for n, body := range tc.files {
				writeFile(t, filepath.Join(mf, n), body)
			}
			g(t, mf, "init", "-q", "--initial-branch=master")
			g(t, mf, "add", "-A")
			g(t, mf, "commit", "-q", "-m", "m")
			ws := filepath.Join(root, "ws")
			os.MkdirAll(ws, 0o755)
			code, out, se := runSplit(t, ws, "init", "-u", mf, "-m", "default.xml")
			if code == 0 {
				t.Fatalf("a broken manifest must not initialize: %q", out)
			}
			all := out + se
			if strings.Contains(all, "panic:") || strings.Contains(all, "goroutine") {
				t.Errorf("crashed instead of reporting the problem: %s", all)
			}
			if !strings.Contains(all, tc.want) {
				t.Errorf("want %q in the error, got: %s", tc.want, all)
			}
			// 失败要清干净:留下半个 .minirepo/ 会让下一次 init 说"已经初始化过了"
			if _, err := os.Stat(filepath.Join(ws, toolDir)); err == nil {
				if code, _, se2 := runSplit(t, ws, "list"); code == 0 {
					t.Logf("note: .minirepo/ left behind after a failed init (%s)", se2)
				}
			}
		})
	}
}

// 8. 边界要**说出来**。未支持的清单特性被静默忽略,是最难查的一类失败(用户以为
// linkfile 生效了)。警告只能进 stderr、整个进程一次,且要列出具体是什么。
func TestUnsupportedFeaturesAreAnnounced(t *testing.T) {
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/" review="https://example.invalid"/>
  <default remote="o" revision="master" sync-j="8"/>
  <project name="proj" path="proj" groups="platform,foo">
    <linkfile src="f" dest="../linked-f"/>
    <annotation name="meta" value="1"/>
  </project>
</manifest>`)
	code, out, se := runSplit(t, ws, "list")
	if code != 0 {
		t.Fatalf("list must still work on a partially unsupported manifest: %s", se)
	}
	if !strings.Contains(out, "proj") {
		t.Errorf("supported part of the manifest must still be honoured: %q", out)
	}
	low := strings.ToLower(se)
	for _, want := range []string{"linkfile", "groups", "sync-j", "review", "annotation", "ignored"} {
		if !strings.Contains(low, want) {
			t.Errorf("warning must name %q so the user can fix the manifest; stderr=%q", want, se)
		}
	}
	if strings.Contains(out, "ignored") {
		t.Error("the warning must not pollute stdout (list --paths is machine readable)")
	}
	if n := strings.Count(se, "ignored"); n > 1 {
		t.Errorf("warnings must be printed once per invocation, got %d", n)
	}
}

// 9. 管道里的输出必须是干净的:着色只在真终端上开(或者设了 NO_COLOR 时永久关)。
// 这条以前只写在注释里,谁也没验过 —— 而 "minirepo status | grep FAIL" 是日常用法。
func TestPipedOutputHasNoAnsi(t *testing.T) {
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master" sync-c="true"/>
  <project name="proj" path="proj"/>
</manifest>`)
	code, out, se := runSplit(t, ws, "sync")
	if code != 0 {
		t.Fatalf("sync: %s", se)
	}
	writeFile(t, filepath.Join(ws, "proj", "f"), "dirty\n")
	code, out, _ = runSplit(t, ws, "status")
	if code != 0 {
		t.Fatalf("status exit code = %d, want 0 (dirty is not missing)", code)
	}
	if strings.Contains(out, "\033[") {
		t.Errorf("status piped into a pipe still emitted ANSI escapes: %q", firstLine(out))
	}
	if !strings.Contains(out, "[dirty]") {
		t.Errorf("uncolored output must still carry the state: %q", out)
	}
	// 反向:确认着色代码真的在(否则上面的断言只是"根本没实现"的假通过)
	origColor, origTTY := noColor, isTTY
	noColor, isTTY = false, func() bool { return true } // 假装在终端前
	t.Cleanup(func() { noColor, isTTY = origColor, origTTY })
	if got := colorize("FAIL", "31"); !strings.Contains(got, "\033[31m") {
		t.Errorf("colorize produced no code with color forced on (%q): the pipe test above is vacuous", got)
	}
	// NO_COLOR 必须赢过 TTY 判断之外的所有情形
	noColor = true
	if got := colorize("FAIL", "31"); got != "FAIL" {
		t.Errorf("NO_COLOR ignored: %q", got)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestCacheEpermNamesThePath 钉住错误质量的一条下限:我们**自己**的缓存目录不可写时
// (权限、只读挂载、磁盘满都是同一类),失败行必须指名是哪个文件 —— 用户从路径就能
// 看出"这是 .minirepo/dl 的权限问题",而不是去怀疑网络或清单。
// 修这个消息时要小心:路径名就是答案,不要用"cache error"这种概括把它糊掉。
func TestCacheEpermNamesThePath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("NOT a pass: root ignores file permissions, so EACCES cannot be simulated")
	}
	_, ws, _ := gitManifestWS(t, `<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
</manifest>`)
	dl := filepath.Join(ws, toolDir, downloadsDir)
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个真实的小归档,让 download() 真正走到"写 .part"那一步
	arch := filepath.Join(ws, "..", "pkg.tar.gz")
	data, _ := makeArchive(t, "tar.gz", "x\n")
	if err := os.WriteFile(arch, data, 0o644); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`<manifest>
  <remote name="o" fetch="../up/"/>
  <default remote="o" revision="master"/>
  <archive url=%q path="out" strip="1"/>
</manifest>`, filepath.ToSlash(arch))
	writeFile(t, filepath.Join(ws, toolDir, "manifests", "default.xml"), body)

	if err := os.Chmod(dl, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dl, 0o700) })

	code, out, _ := runSplit(t, ws, "sync")
	if code == 0 {
		t.Fatal("sync succeeded with an unwritable download cache")
	}
	if !strings.Contains(out, "permission denied") || !strings.Contains(out, ".part") {
		t.Errorf("the failure must name the file and the OS reason, got %q", out)
	}
}
