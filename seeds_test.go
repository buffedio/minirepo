package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 种子语料:每一条对应一条契约条款或一类真实见过的坏输入。fuzz 从这里开始变异,
// 常规 `go test` 也会逐条跑一遍(FuzzX 在 go test 下会执行种子)。
//
// 写这批 seed 时踩到过一个元问题:越界类的 seed 如果没给合法 remote,会先死在
// "no remote",于是"路径防线是否生效"根本没人验证过。seedWant 就是为了堵这个:
// **被拒的原因必须就是这条 seed 想测的那件事**。

// okManifest 是一份"除了被测那一点以外都合法"的清单模板:越界/坏 URL 的 seed 都在
// 它上面改,保证能走到真正的防线,而不是先死在缺 remote。
func okManifest(mutate string) string {
	return `<manifest>
  <remote name="o" fetch="https://host.example/"/>
  <default revision="main" remote="o"/>
  ` + mutate + `
</manifest>`
}

var seedManifests = []struct{ name, body string }{
	// ---- 应当接受 ----
	{"valid", `<manifest>
  <remote name="o" fetch="https://host.example/"/>
  <default revision="main" remote="o"/>
  <project name="a" path="a"/>
  <archive url="https://host.example/x.tar.gz" path="x" sha256="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"/>
</manifest>`},
	{"empty-manifest", `<manifest/>`},
	{"include-child", `<manifest><include name="child.xml"/></manifest>`},
	{"bom", "\xEF\xBB\xBF" + okManifest(`<project name="a" path="a"/>`)},
	{"namespaced-attrs", okManifest(`<project m:name="ignored" name="a" path="a" xmlns:m="http://example.com"/>`)},
	{"unknown-elements", okManifest(`<notice msg="hi"/><superproject name="x" remote="y"/>
	  <project name="a" path="a" upstream="r" groups="g" clone-depth="1" linkfile="l"/>`)},
	{"dup-project-name", okManifest(`<project name="a" path="a"/><project name="a" path="b"/>`)},
	{"dup-remote-name", `<manifest><remote name="o" fetch="https://h1/"/><remote name="o" fetch="https://h2/"/><default revision="r" remote="o"/><project name="a" path="a"/></manifest>`},
	{"archive-no-sha", okManifest(`<archive url="https://h/a.tgz" path="x"/>`)},
	{"archive-strip-negative", okManifest(`<archive url="https://h/a.tgz" path="x" strip="-1"/>`)},
	{"archive-relative-url", okManifest(`<archive url="pkgs/a.tgz" path="x"/>`)},
	{"windows-drive-url", okManifest(`<project name="a" path="a" url="C:\repos\a"/>`)},
	{"entity-reference", okManifest(`<project name="a" path="a" revision="&amp;x"/>`)},
	{"invalid-utf8-name", okManifest(`<project name="` + "\xff\xfe" + `" path="a"/>`)},
	{"deep-include-fanout", okManifest(strings.Repeat(`<include name="child.xml"/>`, 40))},

	// ---- 必须被拒,且拒的理由要指名 ----
	{"wrong-root", `<manifestz><project name="a" path="a" revision="r"/></manifestz>`},
	{"no-root-element", `this is not xml at all, just text mentioning <project>`},
	{"empty", ``},
	{"truncated-tag", `<manifest><project name="a" path="a" revision="r"`},
	{"c0-control-in-attr", okManifest("<project name=\"a\x00b\" path=\"a\"/>")},
	{"self-include", `<manifest><include name="default.xml"/></manifest>`},
	{"include-dotdot", `<manifest><include name="../../etc/passwd"/></manifest>`},
	{"include-absolute-outside", `<manifest><include name="/etc/passwd"/></manifest>`},
	{"project-path-escape", okManifest(`<project name="a" path="../../etc/x"/>`)},
	{"project-path-absolute", okManifest(`<project name="a" path="/etc/x"/>`)},
	{"project-path-backslash-escape", okManifest(`<project name="a" path="a\..\..\etc\x"/>`)},
	{"project-path-dot", okManifest(`<project name="a" path="."/>`)},
	{"copyfile-src-escape", okManifest(`<project name="a" path="a"><copyfile src="../../etc/passwd" dest="out"/></project>`)},
	{"remote-fetch-ext", `<manifest><remote name="o" fetch="ext::sh -c 'id'"/><default revision="r" remote="o"/><project name="a" path="a"/></manifest>`},
	{"project-url-ext", okManifest(`<project name="a" path="a" url="ext::sh -c 'id'"/>`)},
	{"project-url-svn", okManifest(`<project name="a" path="a" url="svn://host/a"/>`)},
	{"project-url-dash", okManifest(`<project name="a" path="a" url="--upload-pack=touch /tmp/pwn"/>`)},
	{"archive-url-ext", okManifest(`<archive url="ext::sh -c 'rm -rf $HOME'" path="x"/>`)},
	{"archive-url-svn", okManifest(`<archive url="svn://host/a.tgz" path="x"/>`)},
	{"archive-path-escape", okManifest(`<archive url="https://h/a.tgz" path="../../etc/x"/>`)},
	{"no-revision", `<manifest><project name="a" path="a"/></manifest>`},
	{"unknown-remote", `<manifest><default revision="r" remote="ghost"/><project name="a" path="a"/></manifest>`},
}

// seedWant:每条 seed 被拒绝时**必须**给出的原因(片段取自实测消息)。
// 不在表里的 seed 表示"应当被接受" —— 那时任何拒绝都算失败,否则断言会被一个
// 不相干的早退悄悄跳过(这正是之前那批 seed 空转的原因)。
var seedWant = map[string]string{
	"wrong-root":                    "root element",
	"no-root-element":               "root element",
	"empty":                         "root element",
	"truncated-tag":                 "malformed manifest",
	"c0-control-in-attr":            "malformed manifest",
	"invalid-utf8-name":             "malformed manifest",
	"self-include":                  "circular",
	"include-dotdot":                "escapes the manifests repo",
	"include-absolute-outside":      "not found",
	"project-path-escape":           "inside the workspace",
	"project-path-absolute":         "inside the workspace",
	"project-path-backslash-escape": "inside the workspace",
	"project-path-dot":              "inside the workspace",
	"copyfile-src-escape":           "inside the workspace",
	"remote-fetch-ext":              "not allowed",
	"project-url-ext":               "not allowed",
	"project-url-svn":               "unsupported transport",
	"archive-url-ext":               "not allowed",
	"archive-url-svn":               "unsupported transport",
	"archive-path-escape":           "inside the workspace",
	"archive-strip-negative":        "not a number",
	"no-revision":                   "revision",
	"unknown-remote":                "remote",
}

// 没列在这里的 seed 都表示"应当被接受"。两个反直觉的要说明一下:
//   - project-url-dash: url="--upload-pack=..." 会被 absolutize 锚到 remote 的 fetch
//     之下,不再是"以 - 开头的参数",因此合法。真正要防的形状由不变量负责:最终 URL
//     不得以 '-' 开头。
//   - archive-relative-url / archive-no-sha: 缺 sha256 只是警告(§7),不是拒绝。
//   - archive-strip-negative: 负数**不**静默回落到默认 1,直接拒(§0 不猜)。

// TestArchiveURLUsesTheSameAllowlist 单独立一条:fuzz 找到的那个洞是"项目过白名单、
// 归档不过"。归档这条路以前一次都没被白名单测过。
func TestArchiveURLUsesTheSameAllowlist(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "default.xml"), okManifest(`<archive url="ext::sh -c 'rm -rf $HOME'" path="x"/>`))
	m := parseManifestFile(dir, "default.xml")
	msg := parseSafely(t, func() { resolveArchives(m, "https://host/mf.git") })
	if !strings.Contains(msg, "archive") || !strings.Contains(msg, "not allowed") {
		t.Errorf("an archive URL must be refused the same way a project URL is, got %q", msg)
	}
	// 反向:合法归档必须原样通过,否则上面那条"报错"可能是恒真
	m2 := mustParse(t, dir, okManifest(`<archive url="https://h/a.tgz" path="x"/>`))
	got := parseSafely(t, func() { resolveArchives(m2, "https://host/mf.git") })
	if got != "" {
		t.Errorf("a plain https archive was refused: %q", got)
	}
}

func mustParse(t *testing.T, dir, body string) *Manifest {
	t.Helper()
	writeFile(t, filepath.Join(dir, "default.xml"), body)
	return parseManifestFile(dir, "default.xml")
}
