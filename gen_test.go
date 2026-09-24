package main

// gen_test.go —— gen 是唯一不依赖清单的命令,所以它的输出必须自己能用:
// 关键性质是 fetch + name == 该仓真实的 origin(repo 的 URL 规则),
// 否则照草稿 init 出来的工作区根本 clone 不到 —— 这条性质以前没人钉过。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkOrigin 建一个真仓,并在另一个仓里把它设为 origin(本地路径,无需网络)
func mkOrigin(t *testing.T, dir string) string {
	t.Helper()
	mkRepo(t, dir)
	return filepath.ToSlash(dir)
}

func genFixture(t *testing.T) (tree, mf string) {
	t.Helper()
	root := t.TempDir()
	tree = filepath.Join(root, "tree")
	mf = filepath.Join(root, "mf")

	// apps/demo:正常分支仓
	demo := filepath.Join(tree, "apps", "demo")
	mkOrigin(t, demo)
	// libs/util:同 host 不同前缀(该拆成两个 remote)
	util := filepath.Join(tree, "libs", "util")
	mkOrigin(t, util)
	// dirty:有本地改动(应出 NOTE,但依然登记)
	dirty := filepath.Join(tree, "dirty")
	mkOrigin(t, dirty)
	os.WriteFile(filepath.Join(dirty, "f"), []byte("v1\nlocal\n"), 0o644)
	// empty:unborn HEAD(没有 commit)
	empty := filepath.Join(tree, "empty")
	os.MkdirAll(empty, 0o755)
	g(t, empty, "init", "-q", "--initial-branch=master")
	// noorigin:没有 origin remote(应跳过并说明原因)
	noorigin := filepath.Join(tree, "noorigin")
	mkRepo(t, noorigin)
	// 噪音目录:不该被当成项目
	os.MkdirAll(filepath.Join(tree, "out", "obj"), 0o755)
	os.WriteFile(filepath.Join(tree, "out", "obj", "x.o"), []byte("o"), 0o644)
	os.MkdirAll(filepath.Join(tree, "node_modules", "pkg"), 0o755)
	mkRepo(t, filepath.Join(tree, "node_modules", "pkg"))

	for _, d := range []string{demo, util, dirty, empty} {
		g(t, d, "remote", "add", "origin", filepath.ToSlash(d))
	}
	_ = noorigin // 故意不给 noorigin 加 origin:它必须被跳过

	os.MkdirAll(mf, 0o755)
	return tree, mf
}

func TestGenDraftIsLoadable(t *testing.T) {
	tree, mf := genFixture(t)
	g(t, mf, "init", "-q", "--initial-branch=master")
	chdir(t, tree)
	out := filepath.Join(mf, "gen.xml")
	cmdGen([]string{"-o", out, tree})

	draft, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(draft)

	// 1. 发现范围
	for _, want := range []string{`path="apps/demo"`, `path="libs/util"`, `path="dirty"`, `path="empty"`} {
		if !strings.Contains(text, want) {
			t.Errorf("draft missing %s", want)
		}
	}
	for _, bad := range []string{"node_modules", `"out`, "noorigin"} {
		if strings.Contains(text, bad) {
			t.Errorf("draft must not contain %q", bad)
		}
	}
	// 2. 不产生无效 XML(注释里不能有 --),且自己的解析器读得回来
	m := parseManifestFile(mf, "gen.xml")
	if len(m.Projects) == 0 {
		t.Fatal("gen output is not loadable by the tool itself")
	}
	// 3. 同前缀共享一个 remote,不同前缀必须拆开(dirty 与 empty 都在 tree/ 下)
	if len(m.Remotes) != 3 {
		t.Errorf("want 3 remotes (tree/, tree/apps/, tree/libs/), got %d: %v", len(m.Remotes), m.Remotes)
	}
	if !strings.Contains(text, "NOTE") {
		t.Error("unborn/dirty projects must be flagged with a NOTE in the draft")
	}
}

func TestGenRoundTripsToRealURLs(t *testing.T) {
	tree, mf := genFixture(t)
	g(t, mf, "init", "-q", "--initial-branch=master")
	chdir(t, tree)
	out := filepath.Join(mf, "gen.xml")
	cmdGen([]string{"-o", out, tree})

	m := parseManifestFile(mf, "gen.xml")
	base := "file://" + mf // 清单仓 URL:相对 fetch 才有锚点,但草稿里 fetch 是绝对路径
	ps := resolveProjects(m, base)

	// 核心性质:fetch + name 必须精确等于该仓真实的 origin
	for _, p := range ps {
		dir := filepath.Join(tree, filepath.FromSlash(p.Path))
		want := g(t, dir, "remote", "get-url", "origin")
		if p.URL != want {
			t.Errorf("project %s: draft URL %q != real origin %q (clone would fail)", p.Path, p.URL, want)
		}
	}
	// unborn 仓给 HEAD 而不是空 revision(空会让 resolve 直接 die)
	for _, p := range ps {
		if p.Revision == "" {
			t.Errorf("project %s has an empty revision", p.Path)
		}
	}
}

// TestGenDraftBuildsAWorkspace 是终点验证:草稿进清单仓 → 新工作区 init+sync → 文件真在
func TestGenDraftBuildsAWorkspace(t *testing.T) {
	tree, mf := genFixture(t)
	g(t, mf, "init", "-q", "--initial-branch=master")
	chdir(t, tree)
	out := filepath.Join(mf, "gen.xml")
	cmdGen([]string{"-o", out, tree})
	g(t, mf, "add", "-A")
	g(t, mf, "commit", "-q", "-m", "draft")

	ws := filepath.Join(filepath.Dir(mf), "ws")
	os.MkdirAll(ws, 0o755)
	chdir(t, ws)
	guard(t)
	cmdInit([]string{"-u", mf, "-m", "gen.xml"})
	// unborn 的 empty 项目带着 revision="HEAD" 占位(草稿里已由 NOTE 点出),
	// 所以这里只同步真正能 clone 的三个
	cmdSync([]string{"apps/demo", "libs/util", "dirty"})

	for _, p := range []string{"apps/demo", "libs/util", "dirty"} {
		if !fileExists(filepath.Join(ws, filepath.FromSlash(p), ".git")) {
			t.Fatalf("%s was not checked out from the gen draft", p)
		}
		if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(p), "f")); err != nil {
			t.Fatalf("%s content missing: %v", p, err)
		}
	}
	// 草稿没有 sync-c,按契约是分离检出 —— 钉在草稿写下的那个 commit 上
	demo := filepath.Join(ws, "apps", "demo")
	if got := g(t, demo, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
		t.Errorf("without sync-c the checkout must be detached, got branch %q", got)
	}
	want := g(t, filepath.Join(tree, "apps", "demo"), "rev-parse", "master")
	if got := g(t, demo, "rev-parse", "HEAD"); got != want {
		t.Errorf("checked out %s, draft says %s", got, want)
	}
}
