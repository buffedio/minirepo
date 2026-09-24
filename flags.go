package main

// flags.go —— 短/长别名。Go 的 flag 包一个名字只注册一次,而用户肌肉记忆来自
// repo(-u 与 --url 同义)。这里集中做别名,保证「真实 CLI 选项」有
// 唯一来源:补全脚本和防漂移测试都从 fs.VisitAll 读同一份清单。

import (
	"errors"
	"flag"
	"regexp"
	"strconv"
	"strings"
)

var errMissingValue = errors.New("missing value")

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

type strVal struct{ p *string }

func (v *strVal) String() string {
	if v == nil || v.p == nil {
		return ""
	}
	return *v.p
}
func (v *strVal) Set(s string) error { *v.p = s; return nil }

type boolVal struct{ p *bool }

// IsBoolFlag 告诉 flag 包这是布尔选项:裸 `-dry-run` 才是 true 而不是
// "flag needs an argument"(自定义 Value 默认必须带值)
func (v *boolVal) IsBoolFlag() bool { return true }

func (v *boolVal) String() string {
	if v == nil || v.p == nil {
		return "false"
	}
	return strconv.FormatBool(*v.p)
}
func (v *boolVal) Set(s string) error {
	b, err := strconv.ParseBool(s)
	if err != nil {
		// `-flag` 后面跟位置参数时 Go 会传空串:视作 true(与 flag.Bool 一致)
		if s == "" {
			*v.p = true
			return nil
		}
		return err
	}
	*v.p = b
	return nil
}

type intVal struct{ p *int }

func (v *intVal) String() string {
	if v == nil || v.p == nil {
		return "0"
	}
	return strconv.Itoa(*v.p)
}
func (v *intVal) Set(s string) error {
	if s == "" {
		return errMissingValue
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*v.p = n
	return nil
}

// str 注册 short(必填)与可选 long 别名,返回共享的指针
// str/boolean/integer 注册 short 与 long 两个名字(空串表示该侧不存在)
func str(fs *flag.FlagSet, short, long string, def, help string) *string {
	p := new(string)
	*p = def
	addStr(fs, short, p, help)
	addStr(fs, long, p, help)
	return p
}

func boolean(fs *flag.FlagSet, short, long, help string) *bool {
	p := new(bool)
	addBool(fs, short, p, help)
	addBool(fs, long, p, help)
	return p
}

func integer(fs *flag.FlagSet, short, long string, def int, help string) *int {
	p := new(int)
	*p = def
	addInt(fs, short, p, help)
	addInt(fs, long, p, help)
	return p
}

func addStr(fs *flag.FlagSet, name string, p *string, help string) {
	if name != "" {
		fs.Var(&strVal{p}, name, help)
	}
}

func addBool(fs *flag.FlagSet, name string, p *bool, help string) {
	if name != "" {
		fs.Var(&boolVal{p}, name, help)
	}
}

func addInt(fs *flag.FlagSet, name string, p *int, help string) {
	if name != "" {
		fs.Var(&intVal{p}, name, help)
	}
}

// parse 解析参数,顺手支持 GNU 式的短选项连写值(-j4 -> -j 4)。
// Go 的 flag 包只认 -j 4 / -j=4,而 repo 与 argparse 用户习惯打 -j4,
// 打错一次不会静默少并行,但会让人以为选项不存在。
func parse(fs *flag.FlagSet, args []string) {
	fixed := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if m := attachedShortRe.FindStringSubmatch(a); m != nil && fs.Lookup(m[1]) != nil {
			fixed = append(fixed, "-"+m[1], m[2])
			continue
		}
		fixed = append(fixed, a)
	}
	fs.Parse(fixed)
}

var attachedShortRe = regexp.MustCompile(`^-([a-zA-Z])([0-9]+)$`)

// requireNoArgs 拒绝多余的位置参数:打错的参数不该无声消失
func requireNoArgs(fs *flag.FlagSet) {
	if extra := fs.Args(); len(extra) > 0 {
		die("%s takes no positional arguments, got: %s", fs.Name(), strings.Join(extra, " "))
	}
}

// boolOf/strOf 在 Parse 之后读回值(布尔选项用 long 名字,短名只是别名)
func boolOf(fs *flag.FlagSet, name string) bool  { return fs.Lookup(name).Value.String() == "true" }
func strOf(fs *flag.FlagSet, name string) string { return fs.Lookup(name).Value.String() }
func intOf(fs *flag.FlagSet, name string) int {
	n, _ := strconv.Atoi(fs.Lookup(name).Value.String())
	return n
}

// flagNames 列出某个 FlagSet 注册的全部选项名(补全/帮助/测试的共同真值)
func flagNames(fs *flag.FlagSet) []string {
	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	return names
}
