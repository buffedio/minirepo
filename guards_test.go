package main

// guards_test.go —— 防漂移守卫。
//
// 这一类 bug 的原始形态是:补全脚本里写着 help,但 argparse/go 分发里根本没注册
// 这个子命令(或反过来)。人肉同步两份清单迟早再犯,所以这里把「宣传的表面」和
// 「真实的表面」对拍:补全脚本、帮助文本、CLI 注册三边必须一致。
//
// 另一条规则:运行时字符串(会进终端/管道/生成的 shell 配置)保持 ASCII;
// 注释可以中文。用 scanner 扫,不靠自觉。

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// builtinFlags 是 Go flag 包/主分发隐式提供的,不需要在 FlagSet 里注册
var builtinFlags = map[string]bool{"h": true, "help": true, "version": true}

func commandFlags() map[string][]string {
	initFS, _ := initFlags()
	forallFS, _, _ := forallFlags()
	return map[string][]string{
		"init":     flagNames(initFS),
		"sync":     flagNames(syncFlags()),
		"list":     flagNames(listFlags()),
		"status":   flagNames(statusFlags()),
		"forall":   flagNames(forallFS),
		"manifest": flagNames(manifestFlags()),
		"freeze":   flagNames(outFlags("freeze")),
		"gen":      flagNames(outFlags("gen")),
		"complete": flagNames(completeFlags()),
		"help":     nil, // 只吃命令名
	}
}

func known(cmd, flag string) bool {
	if builtinFlags[flag] {
		return true
	}
	for _, n := range commandFlags()[cmd] {
		if n == flag {
			return true
		}
	}
	return false
}

// TestCompletionOnlyAdvertisesRealOptions 解析 bash 脚本每个子命令分支里
// compgen -W 宣传的选项,逐个回查真实注册表
func TestCompletionOnlyAdvertisesRealOptions(t *testing.T) {
	script := mustCompletion("bash")
	blocks := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(script, "\n") {
		if m := regexp.MustCompile(`^([a-z]+)\)`).FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if _, ok := commandFlags()[m[1]]; ok {
				cur = m[1]
			} else {
				cur = ""
			}
		}
		if cur == "" {
			continue
		}
		for _, list := range regexp.MustCompile(`compgen -W "([^"]*)"`).FindAllStringSubmatch(line, -1) {
			blocks[cur] = append(blocks[cur], strings.Fields(list[1])...)
		}
	}
	if len(blocks) < 6 {
		t.Fatalf("only found completion blocks for %d subcommands", len(blocks))
	}
	var bad []string
	for cmd, toks := range blocks {
		for _, tk := range toks {
			if !strings.HasPrefix(tk, "-") {
				continue // 位置参数候选(项目名等),不是选项
			}
			name := strings.TrimLeft(tk, "-")
			if name == "" || !known(cmd, name) {
				bad = append(bad, fmt.Sprintf("%s: %s", cmd, tk))
			}
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("completion advertises options the CLI does not have: %v", bad)
	}
}

// TestEverySubcommandIsCovered 真实子命令都得在补全的 SUBS 里(漏了=补不出来),
// 反过来 SUBS 里每个名字都必须是真命令(help bug 的原型)
func TestSubcommandListsAgree(t *testing.T) {
	m := regexp.MustCompile(`local SUBS="([^"]*)"`).FindStringSubmatch(mustCompletion("bash"))
	if m == nil {
		t.Fatal("no SUBS list found in the bash completion script")
	}
	advertised := strings.Fields(m[1])
	real := []string{}
	for c := range commandFlags() {
		real = append(real, c)
	}
	sort.Strings(advertised)
	sort.Strings(real)
	if strings.Join(advertised, " ") != strings.Join(real, " ") {
		t.Errorf("completion SUBS=%v but the CLI has %v", advertised, real)
	}
	for _, c := range advertised {
		if rc, out := runForOutput("", "help", c); rc != 0 {
			t.Errorf("minirepo help %s exited %d: %s", c, rc, strings.TrimSpace(out))
		}
	}
	if rc, _ := runForOutput("", "nosuchcommand"); rc == 0 {
		t.Error("an unknown subcommand must not exit 0")
	}
}

// TestCompletionShellListIsNotHandWritten --shell 的候选必须来自 shells 变量
func TestCompletionShellListIsNotHandWritten(t *testing.T) {
	list := regexp.MustCompile(`--shell\|-s\)[^"]*"([^"]*)"`).FindStringSubmatch(mustCompletion("bash"))
	if list == nil {
		t.Fatal("no --shell completion line in the bash script")
	}
	if got, want := strings.Join(strings.Fields(list[1]), ","), strings.Join(shells, ","); got != want {
		t.Errorf("completion offers %q but the tool supports %q", got, want)
	}
	for _, sh := range shells {
		if _, ok := completionScript(sh); !ok {
			t.Errorf("shells lists %q but there is no script for it", sh)
		}
	}
	if _, ok := completionScript("tcsh"); ok {
		t.Error("completionScript accepted a shell that is not in shells")
	}
}

// TestHelpTextOptionsAreReal 帮助文本(usage + help <cmd>)里的选项同样回查
func TestHelpTextOptionsAreReal(t *testing.T) {
	var bad []string
	// 只查「摘要行」:help 正文里常引用别的命令(freeze 让人抄 init --force),
	// 拿它当本命令的选项清单会误报
	quoted := regexp.MustCompile(`"[^"]*"`) // 引号里是选项的值(-c "git status -s"),不是选项
	check := func(cmd, line string) {
		line = quoted.ReplaceAllString(line, "")
		for _, m := range regexp.MustCompile(`(?:^|\s)-{1,2}([a-zA-Z][a-zA-Z0-9-]*)`).FindAllStringSubmatch(line, -1) {
			if !known(cmd, m[1]) {
				bad = append(bad, fmt.Sprintf("%s: -%s", cmd, m[1]))
			}
		}
	}
	for c, txt := range cmdHelpText {
		check(c, strings.SplitN(txt, "\n", 2)[0])
	}
	for _, line := range strings.Split(usageText(), "\n") {
		m := regexp.MustCompile(`(?m)^\s*minirepo (\w+)\s+(.*)$`).FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, ok := commandFlags()[m[1]]; !ok {
			continue // "minirepo version" 之类的顶层行
		}
		check(m[1], m[2])
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("help text advertises options the CLI does not have: %v", bad)
	}
}

// TestReadmeExamplesAreReal README 代码块里的每条 minirepo 调用都拿去对拍真实
// CLI:文档写错一个选项,和补全脚本写错一个选项,是同一种病(两处手写不同步)。
// 语料只取围栏代码块:散文里的 "minirepo 不猜" 之类不该被当命令解析,否则测试
// 会变成天天误报、最后被人忽略的噪音。
func TestReadmeExamplesAreReal(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	inBlock := false
	cmdRe := regexp.MustCompile(`(?:^|[\s&;(])\.{0,2}/?minirepo\s+([a-z][a-z-]*)(.*)$`)
	var bad, missing []string
	seen := map[string]bool{}
	calls := 0
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inBlock = !inBlock
			continue
		}
		if !inBlock || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		m := cmdRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cmd, rest := m[1], strings.TrimSpace(regexp.MustCompile(`"[^"]*"`).ReplaceAllString(m[2], ""))
		if cmd == "version" {
			seen[cmd] = true
			continue
		}
		if _, ok := commandFlags()[cmd]; !ok {
			missing = append(missing, cmd)
			continue
		}
		seen[cmd] = true
		calls++
		for _, f := range regexp.MustCompile(`(?:^|\s)-{1,2}([a-zA-Z][a-zA-Z0-9-]*)`).FindAllStringSubmatch(rest, -1) {
			if !known(cmd, f[1]) {
				bad = append(bad, fmt.Sprintf("minirepo %s: -%s", cmd, f[1]))
			}
		}
	}
	if calls < 8 {
		t.Fatalf("only parsed %d README invocations -- the corpus extractor is probably broken", calls)
	}
	if len(missing) > 0 {
		t.Errorf("README calls non-existent subcommands: %v", missing)
	}
	if len(bad) > 0 {
		t.Errorf("README advertises options the CLI does not have: %v", bad)
	}
	for c := range commandFlags() {
		if !seen[c] {
			t.Errorf("subcommand %q is never shown in a README code example", c)
		}
	}
}

// TestRuntimeStringsAreASCII 注释可以中文,但字符串字面量不行:
// 它们会打进终端、管道、生成的 shell 配置,ASCII-only 环境不能炸
func TestRuntimeStringsAreASCII(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	var bad []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue // 测试里的中文只给自己看
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var s scanner.Scanner
		s.Init(fset.AddFile(f, fset.Base(), len(src)), src,
			func(pos token.Position, msg string) {
				bad = append(bad, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, msg))
			},
			scanner.ScanComments)
		for {
			pos, tok, lit := s.Scan()
			if tok == token.EOF {
				break
			}
			if tok != token.STRING {
				continue
			}
			v, err := strconv.Unquote(lit)
			if err != nil {
				continue
			}
			for i := 0; i < len(v); i++ {
				if v[i] > 0x7f {
					bad = append(bad, fmt.Sprintf("%s:%d: non-ASCII string literal", f, fset.Position(pos).Line))
					break
				}
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Errorf("runtime strings must stay ASCII (comments may be Chinese): %v", bad[:min(len(bad), 8)])
	}
}

// TestManifestJSONSchemaKeys manifest --json 是给 CI 脚本消费的,键名就是对外契约。
// 之前它靠 encoding/json 的默认行为输出 Go 字段名(Projects/URL),同一份 JSON 里
// 归档又用小写 map 键 —— 两种风格混在一起。这里把键名钉死成小写单数风格。
func TestManifestJSONSchemaKeys(t *testing.T) {
	jsonKeys := func(typ reflect.Type) []string {
		var keys []string
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("json")
			if tag == "" {
				t.Errorf("%s.%s has no json tag (would leak the Go field name %q)",
					typ.Name(), typ.Field(i).Name, typ.Field(i).Name)
				continue
			}
			keys = append(keys, strings.Split(tag, ",")[0])
		}
		return keys
	}
	want := map[string][]string{
		"manifestJSON":        {"manifest", "url", "branch", "projects", "archives"},
		"manifestJSONProject": {"name", "path", "remote", "revision", "url"},
		"manifestJSONArchive": {"path", "url", "sha256", "strip", "format"},
	}
	// 显式展开(避免反射查类型的间接性)
	checks := []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeOf(manifestJSON{}), want["manifestJSON"]},
		{reflect.TypeOf(manifestJSONProject{}), want["manifestJSONProject"]},
		{reflect.TypeOf(manifestJSONArchive{}), want["manifestJSONArchive"]},
	}
	for _, ck := range checks {
		if got := jsonKeys(ck.typ); strings.Join(got, ",") != strings.Join(ck.want, ",") {
			t.Errorf("%s json keys = %v, want %v", ck.typ.Name(), got, ck.want)
		}
	}
	// 全小写:出现大写的键说明有人在用 Go 字段名
	for _, ck := range checks {
		for i := 0; i < ck.typ.NumField(); i++ {
			tag := strings.Split(ck.typ.Field(i).Tag.Get("json"), ",")[0]
			if tag != strings.ToLower(tag) {
				t.Errorf("%s.%s -> json key %q is not lowercase", ck.typ.Name(), ck.typ.Field(i).Name, tag)
			}
		}
	}
}
