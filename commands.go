package main

// commands.go —— init / list / status / forall / manifest / freeze / gen

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// ---------- init ----------

type initOpts struct {
	url, manifest, branch *string
	force                 *bool
}

func initFlags() (*flag.FlagSet, *initOpts) {
	fs := newFlagSet("init")
	o := &initOpts{
		url:      str(fs, "u", "url", "", "manifests repository URL (or local path)"),
		manifest: str(fs, "m", "manifest", "", "manifest file name inside that repository"),
		branch:   str(fs, "b", "branch", "", "manifests repository branch"),
		force:    boolean(fs, "", "force", "re-initialize (keeps the download cache)"),
	}
	return fs, o
}

func cmdInit(args []string) {
	fs, o := initFlags()
	parse(fs, args)
	requireNoArgs(fs)
	url, manifest, branch, force := o.url, o.manifest, o.branch, o.force
	if *url == "" || *manifest == "" {
		die("usage: minirepo init -u <manifests repo URL or local path> -m <manifest file> [-b branch] [--force]")
	}
	root, _ := os.Getwd()
	tool := filepath.Join(root, toolDir)

	if fileExists(tool) {
		if !*force {
			die("already initialized (%s/ exists); use --force to re-init", toolDir)
		}
		// --force 重建配置与清单检出,但 dl/ 里可能是几个 GB 的压缩包缓存,
		// 不能一起扔(先挪出去,清完再挪回来)。
		dl := filepath.Join(tool, downloadsDir)
		park := filepath.Join(root, toolDir+"-dl.keep")
		if fileExists(dl) {
			os.RemoveAll(park)
			if err := os.Rename(dl, park); err != nil {
				die("cannot preserve the download cache: %v", err)
			}
			defer func() {
				os.RemoveAll(dl)
				os.Rename(park, dl)
			}()
		}
		if err := os.RemoveAll(tool); err != nil {
			die("cannot remove %s: %v", tool, err)
		}
	}

	safeURL(*url, "-u") // clone 之前就拒掉 ext:: 这类传输
	mdir := filepath.Join(tool, manifestsDir)
	cargs := []string{"clone", "-c", "advice.detachedHead=false"}
	if *branch != "" {
		cargs = append(cargs, "-b", *branch)
	}
	cargs = append(cargs, "--", *url, mdir)
	fmt.Printf("Cloning manifests into %s ...\n", filepath.ToSlash(filepath.Join(toolDir, manifestsDir)))
	if err := gitRun("", cargs...); err != nil {
		os.RemoveAll(tool)
		die("cloning manifests failed (see git output above)")
	}
	if !fileExists(filepath.Join(mdir, filepath.FromSlash(*manifest))) {
		os.RemoveAll(tool)
		die("manifest file %q not found in the manifests repository", *manifest)
	}
	saveConfig(root, &Config{URL: *url, Branch: *branch, Manifest: *manifest})

	// 立刻解析一遍:清单语法错在 init 时就报,而不是等第一次 sync
	_, projects, archives := resolved(root, false)
	fmt.Printf("Initialized workspace with %d project(s), %d archive(s):\n", len(projects), len(archives))
	for _, p := range projects {
		fmt.Printf("  %-32s %-24s %s\n", p.Path, p.Revision, maskURL(p.URL))
	}
	for _, a := range archives {
		fmt.Printf("  %-32s %-24s %s\n", a.Path, "archive", maskURL(a.URL))
	}
	if len(projects) == 0 && len(archives) == 0 {
		// 计数行其实已经写着 0,但没人会逐字看;而"什么都不声明"的清单多半是
		// -m 打错了文件名 —— 那正是"静默 no-op"最贵的地方。
		warn("this manifest declares nothing: check -m %q (a wrong file name still parses)", *manifest)
	}
	fmt.Println("\nNext: minirepo sync")
}

// ---------- list ----------

func listFlags() *flag.FlagSet {
	fs := newFlagSet("list")
	boolean(fs, "", "paths", "print project paths only (machine readable)")
	boolean(fs, "a", "all", "show the resolved URL too")
	return fs
}

func cmdList(args []string) {
	fs := listFlags()
	parse(fs, args)
	paths, all := boolOf(fs, "paths"), boolOf(fs, "all")
	root := mustWorkspace()
	_, projects, archives := resolved(root, false)
	projects, archives = filterProjects(projects, archives, fs.Args())
	// 按路径排序:输出要能被 diff / 补全消费,而清单里的声明顺序不该影响它
	sort.Slice(projects, func(i, j int) bool { return projects[i].Path < projects[j].Path })
	sort.Slice(archives, func(i, j int) bool { return archives[i].Path < archives[j].Path })
	for _, p := range projects {
		switch {
		case paths:
			fmt.Println(p.Path)
		case all:
			fmt.Printf("%-32s %-48s %-10s %s\n", p.Path, p.Name, p.Revision, maskURL(p.URL))
		default:
			fmt.Printf("%-32s %-48s %s\n", p.Path, p.Name, p.Revision)
		}
	}
	for _, a := range archives {
		if paths {
			fmt.Println(a.Path)
			continue
		}
		fmt.Printf("%-32s %-48s %-10s %s\n", a.Path, a.Path+" [archive]", "archive", maskURL(a.URL))
	}
}

// ---------- status ----------

func statusFlags() *flag.FlagSet { return newFlagSet("status") }

func cmdStatus(args []string) {
	fs := statusFlags()
	parse(fs, args)
	root := mustWorkspace()
	_, projects, archives := resolved(root, false)
	projects, archives = filterProjects(projects, archives, fs.Args())

	type row struct {
		path, state, branch, head string
	}
	one := func(p Project) row {
		dest := filepath.Join(root, filepath.FromSlash(p.Path))
		if !fileExists(filepath.Join(dest, ".git")) {
			return row{p.Path, "MISSING", "", ""}
		}
		br, _ := gitOut(dest, "rev-parse", "--abbrev-ref", "HEAD")
		if br == "HEAD" {
			br = "(detached)"
		}
		head, _ := gitOut(dest, "log", "-1", "--format=%h %s")
		st, _ := gitOut(dest, "status", "--porcelain", "--untracked-files=no")
		state := "clean"
		if st != "" {
			state = "dirty"
		}
		// 与清单的偏差:tag/branch 都解析成 commit 再比,detached 不误报
		if want, err := gitOut(dest, "rev-parse", "--verify", "--quiet", p.Revision+"^{commit}"); err == nil {
			if at, _ := gitOut(dest, "rev-parse", "HEAD"); at != want {
				head += "  [!] manifest wants " + p.Revision
			}
		}
		return row{p.Path, state, br, head}
	}

	var rows []row
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, p := range projects {
		wg.Add(1)
		go func(p Project) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := one(p)
			mu.Lock()
			rows = append(rows, r)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	missing := 0
	for _, r := range rows {
		if r.state == "MISSING" {
			missing++
			fmt.Printf("  [%s] %s\n", colorize("MISSING", "31"), r.path)
			continue
		}
		code := "32"
		if r.state == "dirty" {
			code = "33"
		}
		fmt.Printf("  [%s] %-32s %-12s %s\n", colorize(r.state, code), r.path, r.branch, r.head)
	}
	for _, a := range archives {
		if fileExists(filepath.Join(root, filepath.FromSlash(a.Path))) {
			fmt.Printf("  [%s] %-32s %-12s %s\n", colorize("clean", "32"), a.Path, "archive", maskURL(a.URL))
		} else {
			missing++
			fmt.Printf("  [%s] %s\n", colorize("MISSING", "31"), a.Path)
		}
	}
	if missing > 0 {
		exitNow(1)
	}
}

// ---------- forall ----------

func forallFlags() (*flag.FlagSet, *string, *int) {
	fs := newFlagSet("forall")
	cmdline := str(fs, "c", "command", "", "command to run in every project")
	jobs := integer(fs, "j", "jobs", 4, "parallel jobs")
	return fs, cmdline, jobs
}

func cmdForall(args []string) {
	fs, cmdline, jobs := forallFlags()
	parse(fs, args)
	if *cmdline == "" {
		die("usage: minirepo forall -c <command> [-p] [-j N] [project...]")
	}
	if strings.Contains(*cmdline, "$REPO_") {
		warn("repo exports $REPO_PATH/$REPO_PROJECT/... into -c commands; minirepo does not -- the command already runs inside each project dir")
	}
	root := mustWorkspace()
	_, projects, _ := resolved(root, false)
	projects, _ = filterProjects(projects, nil, fs.Args())

	shell, shellArg := shellFor(runtime.GOOS)
	var mu sync.Mutex
	wg := sync.WaitGroup{}
	sem := make(chan struct{}, max(1, *jobs))
	rc := 0
	for _, p := range projects {
		wg.Add(1)
		go func(p Project) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cmd := exec.Command(shell, shellArg, *cmdline)
			cmd.Dir = filepath.Join(root, filepath.FromSlash(p.Path))
			out, err := cmd.CombinedOutput()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// 失败才带项目名头(对齐 repo:stdout 保持机器可读)
				fmt.Fprintf(os.Stderr, "=== %s ===\n", p.Path)
				os.Stderr.Write(out)
				fmt.Fprintf(os.Stderr, "=== %s: exit %v ===\n", p.Path, exitOf(err))
				rc = 1
				return
			}
			os.Stdout.Write(out)
			if n := len(out); n == 0 || out[n-1] != '\n' {
				fmt.Println()
			}
		}(p)
	}
	wg.Wait()
	if rc != 0 {
		exitNow(rc)
	}
}

func shellFor(goos string) (string, string) {
	if goos == "windows" {
		return os.Getenv("COMSPEC"), "/c" // cmd.exe;README 示例的单引号要换双引号
	}
	return "sh", "-c"
}

func exitOf(err error) string {
	if ee, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("%d", ee.ExitCode())
	}
	return err.Error()
}

// ---------- manifest ----------

func manifestFlags() *flag.FlagSet {
	fs := newFlagSet("manifest")
	boolean(fs, "", "json", "machine readable JSON")
	return fs
}

// manifest --json 的输出结构。键名写死在这里(而不是让 encoding/json 用 Go 字段名),
// 因为这份 JSON 是给 CI 脚本消费的:schema 必须稳定、小写、可自述。
// TestManifestJSONSchemaKeys 钉住键名,防止改字段时顺手改掉对外契约。
type manifestJSONProject struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Remote   string `json:"remote"`
	Revision string `json:"revision"`
	URL      string `json:"url"`
}

type manifestJSONArchive struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Strip  int    `json:"strip"`
	Format string `json:"format,omitempty"`
}

type manifestJSON struct {
	Manifest string                `json:"manifest"`
	URL      string                `json:"url"`
	Branch   string                `json:"branch"`
	Projects []manifestJSONProject `json:"projects"`
	Archives []manifestJSONArchive `json:"archives"`
}

func cmdManifest(args []string) {
	fs := manifestFlags()
	parse(fs, args)
	requireNoArgs(fs)
	asJSON := boolOf(fs, "json")
	root := mustWorkspace()
	cfg, projects, archives := resolved(root, false)

	if asJSON {
		// 展示层统一过 mask:相对 <remote fetch> 会把清单仓 URL 的凭据带进
		// 每条 project/archive URL,只挡顶层 url 挡不住
		o := manifestJSON{Manifest: cfg.Manifest, URL: maskURL(cfg.URL), Branch: cfg.Branch}
		for _, p := range projects {
			o.Projects = append(o.Projects, manifestJSONProject{p.Name, p.Path, p.Remote, p.Revision, maskURL(p.URL)})
		}
		for _, a := range archives {
			o.Archives = append(o.Archives, manifestJSONArchive{a.Path, maskURL(a.URL), a.SHA256, a.Strip, a.Format})
		}
		data, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			die("cannot encode json: %v", err)
		}
		os.Stdout.Write(data)
		fmt.Println()
		return
	}
	fmt.Printf("workspace:  %s\n", root)
	fmt.Printf("manifest:   %s\n", cfg.Manifest)
	loc := maskURL(cfg.URL)
	if cfg.Branch != "" {
		loc += "  (branch " + cfg.Branch + ")"
	}
	fmt.Printf("source:     %s\n", loc)
	mdir := filepath.Join(root, toolDir, manifestsDir)
	if rev, err := gitOut(mdir, "rev-parse", "--short", "HEAD"); err == nil {
		fmt.Printf("revision:   %s %s\n", rev, gitOutOr(mdir, "log", "-1", "--format=%s"))
	}
	if st, _ := gitOut(mdir, "status", "--porcelain"); st != "" {
		fmt.Println("modified:   yes (local edits in the manifests checkout)")
	} else {
		fmt.Println("modified:   no")
	}
	for _, p := range projects {
		fmt.Printf("  %-32s %-24s %s\n", p.Path, p.Revision, maskURL(p.URL))
	}
}

func gitOutOr(dir string, args ...string) string {
	out, _ := gitOut(dir, args...)
	return out
}

// ---------- freeze ----------

// quoteAttr XML 属性引号:含 " 时换 ' 包裹并转义;注释里不能出现 -- (XML 语法)
func quoteAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	if !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, `'`, "&apos;") + "'"
}

func outFlags(name string) *flag.FlagSet {
	fs := newFlagSet(name)
	str(fs, "o", "output", "", "write to FILE instead of stdout")
	return fs
}

func cmdFreeze(args []string) {
	fs := outFlags("freeze")
	parse(fs, args)
	requireNoArgs(fs)
	out := strOf(fs, "output")
	root, cfg := findRoot()
	mdir := filepath.Join(root, toolDir, manifestsDir)
	m := parseManifestFile(mdir, cfg.Manifest)
	base := manifestsBase(cfg.URL, root)
	projects, archives := resolveProjects(m, base), resolveArchives(m, base)

	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!-- Frozen snapshot generated by "minirepo freeze"; do not edit by hand. -->`,
		`<manifest>`,
	}
	// remote 原样写回(保留相对 fetch:快照放在同一清单仓内仍然可解析)
	names := make([]string, 0, len(m.Remotes))
	for n := range m.Remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("  <remote name=%s fetch=%s />", quoteAttr(name), quoteAttr(m.Remotes[name])))
	}
	for _, p := range projects {
		dest := filepath.Join(root, filepath.FromSlash(p.Path))
		if !fileExists(filepath.Join(dest, ".git")) {
			die("project '%s' is not checked out; run sync first", p.Path)
		}
		sha, err := gitOut(dest, "rev-parse", "HEAD")
		if err != nil {
			die("cannot resolve HEAD of '%s'", p.Path)
		}
		line := fmt.Sprintf("  <project name=%s path=%s", quoteAttr(p.Name), quoteAttr(p.Path))
		if p.Remote != "" { // 保留 remote:相对 fetch 的快照在同清单仓内仍可解析
			line += fmt.Sprintf(" remote=%s", quoteAttr(p.Remote))
		}
		line += fmt.Sprintf(" revision=%s", quoteAttr(sha))
		if p.URLAttr != "" { // url= 覆盖原样带回,快照才可用
			line += fmt.Sprintf(" url=%s", quoteAttr(p.URLAttr))
		}
		if len(p.Copyfiles) > 0 {
			lines = append(lines, line+" >")
			for _, cf := range p.Copyfiles {
				lines = append(lines, fmt.Sprintf("    <copyfile src=%s dest=%s />", quoteAttr(cf.Src), quoteAttr(cf.Dest)))
			}
			lines = append(lines, "  </project>")
		} else {
			lines = append(lines, line+" />")
		}
	}
	for _, a := range archives {
		if a.SHA256 == "" {
			warn("archive '%s' has no sha256; snapshot is not reproducible", a.Path)
		}
		line := fmt.Sprintf("  <archive url=%s path=%s", quoteAttr(a.URL), quoteAttr(a.Path))
		if a.SHA256 != "" {
			line += fmt.Sprintf(" sha256=%s", quoteAttr(a.SHA256))
		}
		if a.Strip > 0 {
			line += fmt.Sprintf(` strip="%d"`, a.Strip)
		}
		if a.Format != "" {
			line += fmt.Sprintf(" format=%s", quoteAttr(a.Format))
		}
		lines = append(lines, line+" />")
	}
	lines = append(lines, "</manifest>")
	text := strings.Join(lines, "\n") + "\n"

	if out == "" {
		os.Stdout.WriteString(text)
		return
	}
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		die("cannot write %s: %v", out, err)
	}
	fmt.Printf("frozen %d project(s), %d archive(s) -> %s\n", len(projects), len(archives), out)
	fmt.Println("commit this file into the manifests repository, then:")
	fmt.Println("  minirepo init -u <manifests-url> -m <snapshot.xml> --force")
}

// ---------- gen ----------

// gen 逆推清单草稿:递归扫描目录下的 git 仓,从 origin URL 推断 remote。
// 每仓一次 git 调用拿齐 branch/sha/dirty(porcelain=v2);整棵 SDK 树走下来
// 省掉的子进程是按仓数翻倍的。
func cmdGen(args []string) {
	fs := outFlags("gen")
	parse(fs, args)
	dir := fs.Arg(0)
	out := strOf(fs, "output")
	if fs.NArg() != 1 {
		die("usage: minirepo gen [-o FILE] <dir>")
	}
	abs, err := filepath.Abs(dir)
	if err != nil || !fileExists(abs) {
		die("not a directory: %s", dir)
	}

	var remotes []genRemote
	prefixOwner := map[string]string{}
	var projects []genProj
	scanTree(abs, abs, &remotes, prefixOwner, &projects)
	if len(projects) == 0 {
		die("no git repositories found under %s", dir)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].path < projects[j].path })
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].name < remotes[j].name })

	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		// XML 注释里不能出现 "--":草稿必须能被自己的 init -m 读回来
		`<!-- Draft generated by "minirepo gen". REVIEW before use:`,
		`     remote names and fetch prefixes are guessed from origin URLs;`,
		`     revisions are the current branch names, pin SHAs for releases;`,
		`     nested repositories (a repo inside another repo) are kept as is -->`,
		`<manifest>`,
	}
	for _, r := range remotes {
		lines = append(lines, fmt.Sprintf("  <remote name=%s fetch=%s />", quoteAttr(r.name), quoteAttr(r.fetch)))
	}
	notes := 0
	for _, p := range projects {
		if p.note != "" {
			notes++
			lines = append(lines, "  <!-- NOTE: "+xmlSafeComment(p.note)+" -->")
		}
		lines = append(lines, fmt.Sprintf("  <project name=%s path=%s remote=%s revision=%s />",
			quoteAttr(p.name), quoteAttr(p.path), quoteAttr(p.remote), quoteAttr(p.revision)))
	}
	lines = append(lines, "</manifest>")
	text := strings.Join(lines, "\n") + "\n"

	if out == "" {
		os.Stdout.WriteString(text)
		return
	}
	if err := os.WriteFile(out, []byte(text), 0o644); err != nil {
		die("cannot write %s: %v", out, err)
	}
	fmt.Printf("draft manifest -> %s\n", out)
	fmt.Printf("%d repo(s): %d clean, %d need review\n", len(projects), len(projects)-notes, notes)
}

type genProj struct{ name, path, remote, revision, note string }
type genRemote struct{ name, fetch string }

var skipDirs = map[string]bool{
	".git": true, ".minirepo": true, ".repo": true,
	"out": true, "build": true, "dist": true, "node_modules": true, "dl": true,
}

func scanTree(base, dir string, remotes *[]genRemote, prefixOwner map[string]string, projects *[]genProj) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || skipDirs[e.Name()] {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if fileExists(filepath.Join(full, ".git")) {
			// 命中一个仓就不再往里走(nested 仓由 manifest 表达,不重复登记)
			addGenProject(base, full, remotes, prefixOwner, projects)
			continue
		}
		scanTree(base, full, remotes, prefixOwner, projects)
	}
}

func addGenProject(base, full string, remotes *[]genRemote, prefixOwner map[string]string, projects *[]genProj) {
	rel := filepath.ToSlash(mustRel(base, full))
	// 一次调用拿齐 branch + oid + dirty:gen 会走整棵 SDK 树,每仓省下的子进程都要翻倍
	//   # branch.head master  /  # branch.oid <sha|(initial)>  /  非 # 行 = 有改动
	out, err := gitOut(full, "status", "--porcelain=v2", "--branch", "--untracked-files=no")
	if err != nil {
		fmt.Fprintf(os.Stderr, "minirepo gen: skipped %s (git status --porcelain=v2 failed; git 2.12+ needed)\n", rel)
		return
	}
	branch, oid, dirty := "", "", false
	for _, l := range nonEmptyLines(out) {
		switch {
		case strings.HasPrefix(l, "# branch.head "):
			branch = strings.TrimPrefix(l, "# branch.head ")
		case strings.HasPrefix(l, "# branch.oid "):
			oid = strings.TrimPrefix(l, "# branch.oid ")
		case !strings.HasPrefix(l, "#"):
			dirty = true
		}
	}
	rev := branch
	if rev == "(detached)" {
		rev = ""
	}
	if rev == "" {
		rev = oid // 分离头指针:钉 SHA
	}
	note := ""
	if rev == "" || rev == "(initial)" {
		rev = "HEAD" // unborn:给个能 clone 的占位,并在草稿里点出来
		note = "has no commits yet; set revision explicitly before use"
	} else if dirty {
		note = "has local changes (the draft reflects the working tree)"
	}

	origin, err := gitOut(full, "remote", "get-url", "origin")
	if err != nil || origin == "" {
		fmt.Fprintf(os.Stderr, "minirepo gen: skipped %s (no origin remote)\n", rel)
		return
	}
	// repo 拼 URL 的规则是 fetch + name 直拼,所以 name 保留 ".git":
	// 草稿必须能原样还原 origin,否则按草稿 init 之后 clone 不到
	u := strings.TrimSuffix(origin, "/")
	i := strings.LastIndexAny(u, "/:")
	if i <= 0 || i == len(u)-1 {
		fmt.Fprintf(os.Stderr, "minirepo gen: skipped %s (cannot split origin url %q)\n", rel, origin)
		return
	}
	fetch, name := u[:i+1], u[i+1:]
	rname, ok := prefixOwner[fetch]
	if !ok {
		rname = remoteName(fetch)
		for {
			existing, taken := lookupRemote(remotes, rname)
			if !taken || existing == fetch {
				break
			}
			rname = rname + "-2"
		}
		prefixOwner[fetch] = rname
		*remotes = append(*remotes, genRemote{name: rname, fetch: fetch})
	}
	*projects = append(*projects, genProj{name: name, path: rel, remote: rname, revision: rev, note: note})
}

func lookupRemote(remotes *[]genRemote, name string) (string, bool) {
	for _, r := range *remotes {
		if r.name == name {
			return r.fetch, true
		}
	}
	return "", false
}

func mustRel(base, full string) string {
	rel, err := filepath.Rel(base, full)
	if err != nil {
		die("cannot relativize %s under %s", full, base)
	}
	return rel
}

// remoteName 从 fetch 前缀造 remote 名:URL 取 host,本地路径取整条路径
// (路径必须参与命名,否则一台机器上所有本地 origin 都会塌成同一个名字)
func remoteName(fetch string) string {
	src := fetch
	if strings.Contains(src, "://") || strings.Contains(src, "@") {
		if i := strings.Index(src, "://"); i >= 0 {
			src = src[i+3:]
		}
		if i := strings.Index(src, "@"); i >= 0 {
			src = src[i+1:]
		}
		if i := strings.IndexAny(src, "/"); i >= 0 {
			src = src[:i]
		}
	}
	src = strings.Trim(src, "/:\\")
	h := strings.Trim(strings.ToLower(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(src, "-")), "-")
	if h == "" {
		return "origin"
	}
	return h
}

// xmlSafeComment XML 注释不许含 "--",也不许以 '>' 结尾
func xmlSafeComment(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "--", "-"), ">")
}
