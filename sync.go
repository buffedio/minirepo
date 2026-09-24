package main

// sync.go —— 同步引擎:让工作区等于清单,仅此而已。
// 不自动 merge、不自动 rebase:要么 fast-forward,要么拒绝并告诉你怎么办。
// 全部语义见 CONTRACT.md,回归测试在 main_test.go / archive_test.go。

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ---------- 终端着色 ----------
// 两条都关:显式 NO_COLOR,以及 stdout 不是字符设备(管道/重定向/CI)。
// 后者不是"调用方的事":ESC 序列会毁掉 grep/diff 与 CI 日志,而工具默认就该
// 输出机器可读的东西。

var noColor = os.Getenv("NO_COLOR") != ""

// isTTY 是变量而不是函数:注入它才能测试"代码路径真的会输出 ANSI"。
// 否则 TestPipedOutputHasNoAnsi 的反向断言在管道里永远为假,整个测试就成了
// "因为根本没上色,所以管道里没有颜色"的循环论证。
var isTTY = func() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func colorize(s, code string) string {
	if noColor || !isTTY() {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

// gitFailure 把一次失败的 git 调用变成符合 §5 报告契约的**一行**消息。
//
// 以前这些位置写的是 tailLines(out, 3):把 git 输出的尾 3 行原样塞进报告行。两个错:
//  1. "一项目一行"是 sync 报告的契约(人和脚本都在按行读),git 的 fatal 常常是 4 行,
//     续行看上去就像 minirepo 自己在说话 —— 实测出现过 "[FAIL] s are terminated then
//     try again." 这种读不通的行。
//  2. 取**尾**行等于专门把重点丢掉:git 的结论在第一行("fatal: Unable to create
//     '.../index.lock': File exists."),后面是同一句话的换行续写。
//
// 现在:stdout 只留一行结论,完整原文加 "  git: " 前缀走 stderr —— 诊断信息一条不少,
// 两个流各归各位。
func gitFailure(verb, out string) string {
	detail := strings.TrimSpace(out)
	if detail == "" {
		return verb + " failed (git produced no output; exit status says it all)"
	}
	for _, ln := range strings.Split(detail, "\n") {
		fmt.Fprintln(os.Stderr, "  git: "+ln)
	}
	return verb + ": " + gitVerdict(detail)
}

// gitVerdict 挑出那句"结论":优先最后一条 fatal:/error:(fetch 前面照例是进度噪声),
// 否则第一行。
func gitVerdict(out string) string {
	verdict, first := "", ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if first == "" {
			first = ln
		}
		if strings.HasPrefix(ln, "fatal:") || strings.HasPrefix(ln, "error:") {
			verdict = ln
		}
	}
	if verdict != "" {
		return verdict
	}
	return first
}

type syncResult struct {
	path string
	ok   bool
	msg  string
}

func syncFlags() *flag.FlagSet {
	fs := newFlagSet("sync")
	integer(fs, "j", "jobs", 4, "parallel jobs")
	boolean(fs, "", "force", "overwrite local changes (reset --hard)")
	boolean(fs, "", "no-fetch", "offline: only align the checkout, never touch the network")
	boolean(fs, "", "dry-run", "predict what a real sync would do, change nothing")
	boolean(fs, "", "update-manifests", "update the manifests checkout first (resets it)")
	return fs
}

func cmdSync(args []string) {
	fs := syncFlags()
	parse(fs, args)
	jobs := intOf(fs, "jobs")
	force := boolOf(fs, "force")
	noFetch := boolOf(fs, "no-fetch")
	dryRun := boolOf(fs, "dry-run")
	update := boolOf(fs, "update-manifests")

	root := mustWorkspace()
	_, projects, archives := resolved(root, update)
	projects, archives = filterProjects(projects, archives, fs.Args())
	checkPathConflicts(projects, archives)

	// 嵌套声明(a 与 a/b)进同一个池会让子项目抢跑、父项目 clone 报
	// "directory not empty" 并留下永久废墟 —— 按深度分波:父波全部完成
	// 才开子波。扁平清单只有一波,并行度不受影响。
	byDepth := map[int][]Project{}
	maxDepth := 0
	for _, p := range projects {
		d := strings.Count(p.Path, "/")
		byDepth[d] = append(byDepth[d], p)
		if d > maxDepth {
			maxDepth = d
		}
	}
	type item struct {
		archive bool
		pa      Project
		ar      Archive
	}
	waves := [][]item{}
	if len(archives) > 0 {
		w0 := make([]item, 0, len(archives))
		for _, a := range archives {
			w0 = append(w0, item{archive: true, ar: a})
		}
		waves = append(waves, w0)
	}
	for d := 0; d <= maxDepth; d++ {
		wave := make([]item, 0, len(byDepth[d]))
		for _, p := range byDepth[d] {
			wave = append(wave, item{pa: p})
		}
		waves = append(waves, wave)
	}

	var mu sync.Mutex
	var results []syncResult
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, jobs))
	for _, wave := range waves {
		for _, it := range wave {
			wg.Add(1)
			go func(it item) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				var r syncResult
				if it.archive {
					msg, err := syncArchive(root, it.ar, force, noFetch, dryRun)
					r = syncResult{it.ar.Path, err == nil, msgOrErr(msg, err)}
				} else {
					r = syncOne(root, it.pa, force, noFetch, dryRun)
				}
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}(it)
		}
		wg.Wait() // 波内并行,波间串行
	}

	failed := 0
	for _, r := range results {
		if r.ok {
			fmt.Printf("  [%s] %s %s\n", colorize("OK  ", "32"), r.path, r.msg)
		} else {
			failed++
			fmt.Printf("  [%s] %s %s\n", colorize("FAIL", "31"), r.path, r.msg)
		}
	}
	fmt.Printf("%d ok, %d failed\n", len(results)-failed, failed)
	if failed > 0 {
		exitNow(1)
	}
}

func msgOrErr(msg string, err error) string {
	if err != nil {
		return err.Error()
	}
	return msg
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// mustWorkspace 拿工作区根并顺便校验清单可解析(所有非 init 命令共用)
func mustWorkspace() string {
	root, _ := findRoot()
	return root
}

// syncOne 同步单个项目。返回 (path, ok, message)。
func syncOne(root string, p Project, force, noFetch, dryRun bool) syncResult {
	dest := filepath.Join(root, filepath.FromSlash(p.Path))
	cfn := ""
	if len(p.Copyfiles) > 0 {
		cfn = fmt.Sprintf(" (+%d copyfile)", len(p.Copyfiles))
	}

	freshClone := !fileExists(filepath.Join(dest, ".git"))
	if freshClone {
		if entries, _ := os.ReadDir(dest); len(entries) > 0 {
			return syncResult{p.Path, false, "target exists and is not a git repo"}
		}
		if dryRun {
			return syncResult{p.Path, true, "would clone" + cfn}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return syncResult{p.Path, false, err.Error()}
		}
		cargs := []string{"clone", "-c", "advice.detachedHead=false"}
		if p.SyncC && !isSHA(p.Revision) {
			cargs = append(cargs, "-b", p.Revision)
		}
		// "--" 之后才是 URL:清单里的值再也无法被 git 当成选项
		cargs = append(cargs, "--", p.URL, dest)
		if err := gitRun("", cargs...); err != nil {
			return syncResult{p.Path, false, "clone failed (see git output above)"}
		}
	} else if !noFetch {
		if dryRun {
			ok, msg := dryExisting(dest, force, "would fetch+checkout", cfn)
			return syncResult{p.Path, ok, msg}
		}
		// 清单是唯一真相:来源换了(fetch 前缀改写/加 url=/手工 set-url)就纠正,
		// 否则会永远从旧地址拉还报成功。
		if cur, err := gitOut(dest, "remote", "get-url", "origin"); err == nil && cur != "" &&
			strings.TrimSuffix(cur, "/") != strings.TrimSuffix(p.URL, "/") {
			warn("origin URL changed for %s: %s -> %s", p.Path, maskURL(cur), maskURL(p.URL))
			if rc, out := run(dest, "remote", "set-url", "origin", p.URL); rc != 0 {
				return syncResult{p.Path, false, gitFailure("cannot update origin", out)}
			}
		}
		if rc, out := run(dest, "fetch", "origin"); rc != 0 {
			return syncResult{p.Path, false, gitFailure("fetch failed", out)}
		}
	} else if dryRun {
		ok, msg := dryExisting(dest, force, "would checkout (no-fetch)", cfn)
		return syncResult{p.Path, ok, msg}
	}

	// dirty = 已跟踪文件被改(untracked 不算:嵌套项目和构建产物不能挡同步)。
	// 新 clone 必然干净,跳过。
	dirty := false
	if !freshClone {
		st, _ := gitOut(dest, "status", "--porcelain", "--untracked-files=no")
		dirty = st != ""
		if dirty && !force {
			return syncResult{p.Path, false, "local changes; use --force to overwrite"}
		}
	}
	if dryRun { // 干树走到这里:真实 sync 会对齐检出
		return syncResult{p.Path, true, "would fetch+checkout" + cfn}
	}

	if ok, msg := checkout(dest, p, force); !ok {
		return syncResult{p.Path, false, msg}
	}
	if dirty && force {
		run(dest, "reset", "--hard")
	}
	for _, cf := range p.Copyfiles {
		src := filepath.Join(dest, filepath.FromSlash(cf.Src))
		dst := filepath.Join(root, filepath.FromSlash(cf.Dest))
		if fileExists(src) {
			os.MkdirAll(filepath.Dir(dst), 0o755)
			copyFileSync(src, dst)
		}
	}
	msg := "ok"
	if dirty && force {
		msg = "forced"
	}
	if len(p.Copyfiles) > 0 {
		msg += fmt.Sprintf(" (+%d copyfile)", len(p.Copyfiles))
	}
	return syncResult{p.Path, true, msg}
}

// dryExisting --dry-run 对已检出项目的判定:必须和真实 sync 一致 ——
// 脏项目会报 would refuse 并计入失败(exit 1),否则预演就是在撒谎。
func dryExisting(dest string, force bool, what, cfn string) (bool, string) {
	st, _ := gitOut(dest, "status", "--porcelain", "--untracked-files=no")
	if st != "" && !force {
		return false, "would refuse: local changes; use --force to overwrite"
	}
	return true, what + cfn
}

// checkout 对齐 revision。
//
//	分支 + sync-c → 本地跟踪分支(merge --ff-only;分岐拒绝,--force 才 reset)
//	sha/tag/无 sync-c → 分离头指针。分支名跟随 fetch 后的远端 tip:
//	fetch 只动 refs/remotes/origin/*,本地同名分支永远停在 clone 那一刻 ——
//	用它会让工作区永远不前进还报 OK(0.5.2 修掉的 bug)。
func checkout(dest string, p Project, force bool) (bool, string) {
	rev := p.Revision
	if p.SyncC && !isSHA(rev) {
		rref := "origin/" + rev
		if rc, _ := run(dest, "rev-parse", "--verify", "--quiet", rref); rc != 0 {
			// 远端没有这个分支(清单写错/tag 同名):退化为分离检出
			rc, out := run(dest, "checkout", "-f", "--detach", rev)
			if rc != 0 {
				return false, gitFailure("detached checkout failed", out)
			}
			return true, fmt.Sprintf("detached at %s (remote branch absent)", rev)
		}
		cur, _ := gitOut(dest, "rev-parse", "--abbrev-ref", "HEAD")
		if cur == rev {
			rc, out := run(dest, "merge", "--ff-only", rref)
			if rc != 0 {
				if !force {
					return false, fmt.Sprintf("branch '%s' diverged from %s; commit/push or use --force", rev, rref)
				}
				rc, out = run(dest, "reset", "--hard", rref)
				if rc != 0 {
					return false, gitFailure("reset --hard failed", out)
				}
			}
			return true, "branch " + rev
		}
		// 不在该分支上:若本地分支有未推送提交,不许 -B 直接覆盖
		if rc, _ := run(dest, "show-ref", "--verify", "--quiet", "refs/heads/"+rev); rc == 0 && !force {
			cnt, _ := gitOut(dest, "rev-list", "--left-right", "--count", rev+"..."+rref)
			if fields := strings.Fields(cnt); len(fields) == 2 && fields[0] != "0" {
				return false, fmt.Sprintf("local branch '%s' has unpushed commits; use --force to reset", rev)
			}
		}
		rc, out := run(dest, "checkout", "-B", rev, rref)
		if rc != 0 {
			return false, gitFailure("checkout failed", out)
		}
		return true, "branch " + rev
	}

	// 分离检出:分支名优先取远端 tip
	target := rev
	if !isSHA(rev) {
		if rc, _ := run(dest, "rev-parse", "--verify", "--quiet", "origin/"+rev); rc == 0 {
			target = "origin/" + rev
		}
	}
	rc, out := run(dest, "checkout", "-f", "--detach", target)
	if rc != 0 && (strings.Contains(out, "is not a tree") || strings.Contains(out, "did not match") ||
		strings.Contains(out, "ambiguous argument")) {
		// git 只说对象不存在;把通常原因和下一步说出来
		return false, fmt.Sprintf("revision %s is not reachable from any branch or tag of the remote (deleted branch? PR-only commit?) -- fetch it inside the project or fix the manifest revision", rev)
	}
	if rc != 0 {
		return false, gitFailure("checkout failed", out)
	}
	return true, "detached at " + rev
}

func copyFileSync(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()
	buf := make([]byte, 128*1024)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}
