package main

// workspace.go —— 工作区布局、config、git 进程管道
//
// 布局(与旧版工作区兼容,可原地接管):
//   <root>/.minirepo/config.json   工具配置(0600:里面可能有带凭据的 URL)
//   <root>/.minirepo/manifests/    清单仓检出(--update-manifests 时更新)
//   <root>/.minirepo/dl/           压缩包下载缓存(断点续传)
//   <root>/<project path>/         各项目检出

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	toolDir      = ".minirepo"
	manifestsDir = "manifests"
	configFile   = "config.json"
	downloadsDir = "dl"
	archiveStamp = ".minirepo-archive" // 写进解压目录,记录来源归档的 sha
)

// Config 工作区配置。URL 可能带凭据(https://user:token@host/...),所以落盘 0600。
type Config struct {
	URL      string `json:"url"`
	Branch   string `json:"branch,omitempty"`
	Manifest string `json:"manifest"`
}

// ---------- 查找工作区 ----------

// findRoot 从 cwd 向上找 .minirepo/config.json(与 repo 的 .repo 行为一致)
func findRoot() (string, *Config) {
	cur, err := os.Getwd()
	if err != nil {
		die("failed to get current dir: %v", err)
	}
	for {
		cfgPath := filepath.Join(cur, toolDir, configFile)
		if fileExists(cfgPath) {
			cfg := loadConfigFile(cfgPath)
			if cfg.Manifest == "" {
				die("%s is missing the 'manifest' field; re-run `minirepo init`", cfgPath)
			}
			return cur, cfg
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			die("not a minirepo workspace (no %s/ found); run `minirepo init` first", toolDir)
		}
		cur = parent
	}
}

func loadConfigFile(path string) *Config {
	data, err := os.ReadFile(path)
	if err != nil {
		die("cannot read %s: %v", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		die("malformed %s: %v", path, err)
	}
	return &cfg
}

// saveConfig 落盘配置;可能含凭据,固定 0600(Windows 上 chmod 是 no-op,无害)
func saveConfig(root string, cfg *Config) {
	dir := filepath.Join(root, toolDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		die("cannot create %s: %v", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		die("cannot encode config: %v", err)
	}
	path := filepath.Join(dir, configFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		die("cannot write %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = err // Windows / 只读介质:尽力而为
	}
}

// ---------- 清单解析入口 ----------

// manifestsBase 锚定清单仓 URL:init -u ../mf 这种相对路径按工作区根归一,
// 否则相对 <remote fetch> 会在不同子目录下解析出不同结果(老 bug)。
func manifestsBase(url, root string) string {
	if !isRelativeURL(url) {
		return url
	}
	cand := filepath.Join(root, filepath.FromSlash(url))
	if fileExists(cand) {
		return filepath.ToSlash(cand)
	}
	return url
}

// resolved 返回当前工作区完全解析后的 (cfg, projects, archives)。
// updateFirst=true 时先更新清单仓(--update-manifests 语义)。
func resolved(root string, updateFirst bool) (*Config, []Project, []Archive) {
	cfg := loadConfigFile(filepath.Join(root, toolDir, configFile))
	mdir := filepath.Join(root, toolDir, manifestsDir)
	if updateFirst {
		updateManifests(root, mdir, cfg)
	}
	m := parseManifestFile(mdir, cfg.Manifest)
	base := manifestsBase(cfg.URL, root)
	return cfg, resolveProjects(m, base), resolveArchives(m, base)
}

// updateManifests 拉最新清单。会 reset --hard:先 warn 丢弃了多少本地改动,
// 不给"调试时改了清单、一同步就没了"任何惊喜。
func updateManifests(root, mdir string, cfg *Config) {
	if out, _ := gitOut(mdir, "status", "--porcelain"); strings.TrimSpace(out) != "" {
		n := len(nonEmptyLines(out))
		warn("discarding local edits in %s (%d file(s))", filepath.ToSlash(filepath.Join(toolDir, manifestsDir)), n)
	}
	if rc, out := run(mdir, "fetch", "origin"); rc != 0 {
		die("updating manifests failed:\n%s", tailLines(out, 5))
	}
	target := "FETCH_HEAD"
	if cfg.Branch != "" {
		target = "origin/" + cfg.Branch
	}
	if rc, out := run(mdir, "reset", "--hard", target); rc != 0 {
		die("updating manifests failed:\n%s", tailLines(out, 5))
	}
	fmt.Printf("manifests updated to %s\n", target)
}

// ---------- git 管道 ----------

// gitTimeout 是**捕获输出**那类调用(status/list 探测、fetch 判定)的上限。
// 可用 MINIREPO_GIT_TIMEOUT 覆盖(与 NO_COLOR 一样走 env,不加新 flag)。
//
// 注意它**不是**防凭据提示挂死的办法:git 要密码时是直接开 /dev/tty 的,提示会
// 无人应答地等到超时(默认 600s),用户看到的是"minirepo 卡了十分钟"。挂死由
// gitEnv 里的 GIT_TERMINAL_PROMPT 决定,见下。
func gitTimeout() time.Duration {
	if v := os.Getenv("MINIREPO_GIT_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		warn("MINIREPO_GIT_TIMEOUT=%q is not a number, using the default", v)
	}
	return 600 * time.Second
}

// gitEnv 决定 git 子进程**能不能问人要凭据**。三条路共用一处判断,免得各写各的:
//   - 探测类(interactive=false:rev-parse / status / ls-files 等)必须立刻失败:
//     一次 `minirepo status` 绝不该停在"Username for https://..."上。
//   - clone/fetch(interactive=true)允许问人 —— 但仅当输出还在终端上。把 stdout
//     重定向进日志/管道的人,已经表明这不是交互场景。
//   - 大仓库 clone **不设总时长**(38MB 的包与 3GB 的包不该用同一个数;要防的是
//     挂死而不是慢,那是空闲超时和这条 env 的职责)。
func gitEnv(interactive bool) []string {
	env := os.Environ()
	if !interactive || !isTTY() {
		env = append(env, "GIT_TERMINAL_PROMPT=0")
	}
	return env
}

// gitCmd 是 git 子进程的唯一入口(ctx 为空表示不加超时)。
func gitCmd(ctx context.Context, dir string, interactive bool, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if ctx == nil {
		cmd = exec.Command("git", args...)
	} else {
		cmd = exec.CommandContext(ctx, "git", args...)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv(interactive)
	return cmd
}

// run 执行 git 并捕获输出(合并 stdout/stderr)。返回 (exitCode, output)。
// 超时返回 124(与 timeout(1) 一致)。
func run(dir string, args ...string) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout())
	defer cancel()
	out, err := gitCmd(ctx, dir, false, args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return 124, fmt.Sprintf("timeout after %ds: git %s", int(gitTimeout().Seconds()), strings.Join(args, " "))
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	if err != nil {
		return 127, err.Error()
	}
	return 0, string(out)
}

// gitRun 交互式执行 git,stdio 全继承(人要能看见进度、能被问到凭据)。
func gitRun(dir string, args ...string) error {
	cmd := gitCmd(nil, dir, true, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// gitOut 捕获 stdout(探测状态用),TrimSpace 后返回
func gitOut(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout())
	defer cancel()
	out, err := gitCmd(ctx, dir, false, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// ---------- 小工具 ----------

// die/warn 是变量:测试时替换成 panic/静默,(exit 路径才能被单测覆盖)
var die = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "minirepo: error: "+format+"\n", args...)
	os.Exit(1)
}

var warn = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "minirepo: warning: "+format+"\n", args...)
}

// exitNow 命令层的退出码出口;单测里换成 panic,否则一次失败会杀掉整个测试进程
var exitNow = os.Exit

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func tailLines(s string, n int) string {
	lines := nonEmptyLines(s)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// isSHA 是否为 40 位十六进制(fixed SHA)
func isSHA(rev string) bool {
	if len(rev) != 40 {
		return false
	}
	for _, c := range rev {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// maskURL 显示用:藏掉 userinfo。https://user:token@host/m 进终端记录和
// CI 日志的只能是 https://***@host/m;真值在 config(0600)里,clone 不受影响。
var credRe = regexp.MustCompile(`^([a-zA-Z][a-zA-Z0-9+.-]*://)([^/@]+)@`)

func maskURL(url string) string {
	return credRe.ReplaceAllString(url, "${1}***@")
}
