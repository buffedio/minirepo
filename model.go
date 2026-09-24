package main

// model.go —— 解析层:把清单里的相对引用变成可执行的绝对事实。
// 这里集中了全部安全/确定性约束,规则逐条对应 CONTRACT.md。

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Project 完全解析后的项目(URL 绝对、path 规整、revision 必有)
type Project struct {
	Name      string
	Path      string // 工作区内相对路径,统一 '/'
	Remote    string
	Revision  string
	URL       string // 解析后的绝对 URL(本地仓=绝对路径)
	URLAttr   string // 清单里的原始 url 属性(freeze 往返保真用)
	SyncC     bool
	Copyfiles []Copyfile
}

// Archive 压缩包依赖(minirepo 扩展)
type Archive struct {
	URL, Path, SHA256, Format string
	Strip                     int
}

var urlSchemes = map[string]bool{
	"http": true, "https": true, "git": true, "ssh": true, "file": true,
}

// isRelativeURL 无 scheme、无 scp 形态、不以 / 开头、非 Windows 盘符 ——
// repo 的规则:按 manifests 仓 URL 解析。
func isRelativeURL(u string) bool {
	if u == "" {
		return false
	}
	if regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`).MatchString(u) {
		return false
	}
	if regexp.MustCompile(`^[^/\\\s]+@[^/\\\s]+:`).MatchString(u) {
		return false // scp 形态 user@host:path
	}
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, `\\`) {
		return false
	}
	if regexp.MustCompile(`^[A-Za-z]:[/\\]`).MatchString(u) {
		return false // Windows 盘符
	}
	return true
}

// absolutize 相对 fetch/url 按 manifests 仓 URL 解析:.. 掉一段
// (…/pub/manifests.git + ../kernel → …/pub/kernel)。在解析期做,与执行命令
// 的目录无关 —— 这是「工作区任意子目录可用」的前提。
func absolutize(base, url string) string {
	if !isRelativeURL(url) {
		return url
	}
	if base == "" {
		die("manifest uses a relative URL (%s) but the manifests repository URL is unknown; re-run `minirepo init -u <url>`", url)
	}
	if isRelativeURL(base) {
		die("the recorded manifests repo URL (%s) is relative and cannot be anchored; re-run `minirepo init -u <absolute path or URL>`", base)
	}
	target := strings.TrimSuffix(base, "/")
	for _, part := range strings.Split(url, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			i := strings.LastIndex(target, "/")
			if i >= 0 {
				target = target[:i]
			}
			continue
		}
		target += "/" + part
	}
	return target
}

// safeURL 传输白名单:清单是第三方输入,<project url="ext::sh -c ..."> 在
// git<2.38(或被配置打开)就是任意命令执行。只放行常规传输和本地路径,
// 任何 <helper>:: 形态与 - 开头(会被 git 当选项)一律拒绝。
// 三个模式在热路径上(sync 的每个项目/归档都会过一次),必须只编译一次。
var (
	windowsDriveRE = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	extTransportRE = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)::`)
	schemeRE       = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)
)

// classifyURL 把"这个 URL 能不能取"判成 (kind, token):kind 为 "" 表示接受。
// 单独成一个纯函数,是为了让这条白名单能被测试直接查询,而不必靠替换全局 die 去
// 间接观察(safeURL 只是给它套上面向用户的措辞)。
func classifyURL(url string) (kind, token string) {
	u := strings.TrimSpace(url)
	if u == "" || strings.HasPrefix(u, "-") {
		return "empty", u
	}
	if windowsDriveRE.MatchString(u) {
		return "", u // Windows 盘符路径
	}
	if m := extTransportRE.FindStringSubmatch(u); m != nil {
		return "ext", m[1] + "::"
	}
	if sm := schemeRE.FindStringSubmatch(u); sm != nil {
		if !urlSchemes[strings.ToLower(sm[1])] {
			return "scheme", sm[1]
		}
	}
	return "", u
}

func safeURL(url, what string) string {
	kind, token := classifyURL(url)
	switch kind {
	case "ext":
		die("refusing %s: transport %q is not allowed (only %s and local paths are accepted)",
			what, token, "file/git/http/https/ssh")
	case "scheme":
		die("refusing %s: unsupported transport %q in %q", what, token+":", url)
	case "empty":
		die("%s is not a usable URL: %q", what, url)
	}
	return strings.TrimSpace(url)
}

// within 规整工作区相对路径并把它摁在 workspace 里面:
// 绝对路径和任何 .. 分量直接 die(曾实测 ../escape 真把仓 clone 到了工作区外,
// 绝对路径更糟 —— filepath.Join 会整个丢掉 root)。返回统一 '/' 分隔的路径。
func within(path, what string) string {
	parts := []string{}
	for _, c := range strings.Split(strings.ReplaceAll(path, "\\", "/"), "/") {
		if c != "" && c != "." {
			parts = append(parts, c)
		}
	}
	if strings.HasPrefix(path, "/") || filepath.IsAbs(path) ||
		windowsDriveRE.MatchString(path) ||
		containsDotDot(parts) || len(parts) == 0 {
		die("%s must be a path inside the workspace: %q", what, path)
	}
	return strings.Join(parts, "/")
}

func containsDotDot(parts []string) bool {
	for _, p := range parts {
		if p == ".." {
			return true
		}
	}
	return false
}

// resolveProjects 填默认值、锚定 URL、校验边界。revision 是硬约束:
// <project> 和 <default> 都没给就 die —— 不猜 master(main 分支的仓会拿到
// 说不清的失败)。
func resolveProjects(m *Manifest, base string) []Project {
	out := []Project{}
	for _, rp := range m.Projects {
		remote := rp.Remote
		if remote == "" {
			remote = m.Default.Remote
		}
		rev := rp.Revision
		if rev == "" {
			rev = m.Default.Revision
		}
		if rev == "" {
			die("project %s: no revision (set it on <project> or <default revision>)", rp.Name)
		}
		fetch, ok := m.Remotes[remote]
		if !ok {
			if remote == "" {
				die("project %s: no remote (set <default remote> or a per-project remote)", rp.Name)
			}
			die("project %s: remote %q not defined in manifest", rp.Name, remote)
		}
		url := ""
		if rp.URL != "" {
			url = safeURL(absolutize(base, rp.URL), "<project url>")
		} else {
			url = safeURL(absolutize(base, fetch+rp.Name), "<remote fetch>")
		}
		path := rp.Path
		if path == "" {
			path = rp.Name // repo 默认 path=name
		}
		path = within(path, "<project path>")
		syncC := false
		if rp.SyncC != nil {
			syncC = *rp.SyncC
		} else {
			syncC = m.Default.SyncC
		}
		cfs := []Copyfile{}
		for _, cf := range rp.Copyfiles {
			cfs = append(cfs, Copyfile{
				Src:  within(cf.Src, "<copyfile src>"),
				Dest: within(cf.Dest, "<copyfile dest>"),
			})
		}
		out = append(out, Project{
			Name: rp.Name, Path: path, Remote: remote, Revision: rev,
			URL: url, URLAttr: rp.URL, SyncC: syncC, Copyfiles: cfs,
		})
	}
	return out
}

// resolveArchives 归档依赖:URL 锚定(相对 URL 曾按 CWD 解析,子目录里 sync
// 就找不到包)、path 边界、strip 非负已在解析期校验。
func resolveArchives(m *Manifest, base string) []Archive {
	out := []Archive{}
	for _, ra := range m.Archives {
		out = append(out, Archive{
			// 白名单对项目与归档必须一视同仁:否则同一份清单里 <archive url> 就是
			// 一条绕过 §4 的路(fuzz 真找到过:它返回了 "A0:")。
			URL:    safeURL(absolutize(base, ra.URL), "<archive url>"),
			Path:   within(ra.Path, "<archive path>"),
			SHA256: ra.SHA256,
			Format: ra.Format,
			Strip:  ra.Strip,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// pkey 命令行过滤器可用的键:path 与 name,都做尾 '/' 归一
// (sync kernel/ 曾报 unknown project —— 补全给过斜杠的历史包袱)。
func pkey(s string) string {
	return strings.TrimSuffix(strings.ReplaceAll(s, "\\", "/"), "/")
}

// filterProjects 按位置参数过滤;未知名直接报错列出 —— 静默跑 0 个项目
// (老 forall 的行为)只会让人以为同步过了。
func filterProjects(projects []Project, archives []Archive, args []string) ([]Project, []Archive) {
	if len(args) == 0 {
		return projects, archives
	}
	want := map[string]bool{}
	for _, a := range args {
		want[pkey(a)] = true
	}
	known := map[string]bool{}
	for _, p := range projects {
		known[pkey(p.Path)], known[pkey(p.Name)] = true, true
	}
	for _, a := range archives {
		known[pkey(a.Path)] = true
	}
	var unknown []string
	for k := range want {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		die("unknown project(s): %s", strings.Join(unknown, ", "))
	}
	var kp []Project
	for _, p := range projects {
		if want[pkey(p.Path)] || want[pkey(p.Name)] {
			kp = append(kp, p)
		}
	}
	var ka []Archive
	for _, a := range archives {
		if want[pkey(a.Path)] {
			ka = append(ka, a)
		}
	}
	return kp, ka
}

// checkPathConflicts 一个路径一个主人:任何两条声明抢同一个目录都拒绝。
// 报错必须指名**双方是谁**(项目名 / 归档 URL):原先归档之间冲突时消息把对方说成
// "project <路径>",看着像工具搞不清自己声明了什么。
// 嵌套声明(a 与 a/b)是故意允许的(Buildroot 真这么用)。
func checkPathConflicts(projects []Project, archives []Archive) {
	owner := map[string]string{}
	claim := func(path, who string) {
		if prev, ok := owner[path]; ok {
			die("path %q is claimed by both %s and %s", path, prev, who)
		}
		owner[path] = who
	}
	for _, p := range projects {
		claim(p.Path, "project "+p.Name)
	}
	for _, a := range archives {
		claim(a.Path, "archive "+maskURL(a.URL))
	}
}
