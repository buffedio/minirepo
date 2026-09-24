// minirepo —— 精简版多仓库管理工具(单二进制,Google repo 的日常子集)
//
// 清单是 repo XML 的已声明子集(remote/default/project/include/copyfile/
// remove-project + minirepo 的 <archive> 扩展),语义契约见 CONTRACT.md。
// 未知元素/属性会一次性警告后忽略 —— 边界必须可见,不许静默。

package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "init":
		cmdInit(os.Args[2:])
	case "sync":
		cmdSync(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "status", "st":
		cmdStatus(os.Args[2:])
	case "forall":
		cmdForall(os.Args[2:])
	case "manifest":
		cmdManifest(os.Args[2:])
	case "freeze":
		cmdFreeze(os.Args[2:])
	case "gen":
		cmdGen(os.Args[2:])
	case "complete":
		cmdComplete(os.Args[2:])
	case "help", "-h", "--help":
		cmdHelp(os.Args[2:])
	case "-v", "--version", "version":
		// 裸版本串,不带程序名:调用方从 argv[0] 就知道是谁;脚本比较版本时
		// `[ "$("$0" version)" = "v0.10.0" ]` 也少一步剥离。
		fmt.Println(versionString())
	default:
		fmt.Fprintf(os.Stderr, "minirepo: unknown command %q\n\n", os.Args[1])
		usage()
	}
}

var cmdHelpText = map[string]string{
	"init":     "minirepo init -u <manifests repo URL or local path> -m <manifest.xml> [-b branch] [--force]\n\n  The only bootstrap command. Clones the manifests repository into\n  .minirepo/manifests and records the reference in .minirepo/config.json.\n  --force rebuilds config + manifests checkout but keeps the download cache.",
	"sync":     "minirepo sync [-j N] [--force] [--no-fetch] [--dry-run] [--update-manifests] [project...]\n\n  Make the workspace equal the manifest: clone missing projects, fetch,\n  align the checkout. Never merges or rebases -- either fast-forward or\n  refuse with instructions. Nested projects sync by depth (parents first).\n  --dry-run predicts the real outcome, including refusals (exit 1),\n  so `minirepo sync --dry-run && minirepo sync` is safe to chain.",
	"list":     "minirepo list [--paths] [-a] [project...]\n\n  Show the resolved projects. --paths is machine readable (used by shell\n  completion); -a also shows the resolved URL (credentials masked).",
	"status":   "minirepo status [project...]\n\n  Per project: branch, clean/dirty, HEAD, and whether it deviates from\n  the manifest revision.",
	"forall":   "minirepo forall -c <command> [-j N] [project...]\n\n  Run a command in every project directory. Output stays machine readable\n  (no project headers); failures go to stderr with a === path === marker.\n  Does NOT export repo's $REPO_* variables (warned if the command uses one).",
	"manifest": "minirepo manifest [--json]\n\n  Show which manifest is in effect, its source and revision, and the\n  resolved projects. --json is machine readable (credentials masked).",
	"freeze":   "minirepo freeze -o <snapshot.xml>\n\n  Pin every project's current HEAD into a standalone snapshot manifest.\n  Commit it into the manifests repo; anyone can rebuild the exact\n  workspace with `minirepo init -u <url> -m <snapshot.xml> --force`.",
	"gen":      "minirepo gen [-o FILE] <dir>\n\n  Reverse-engineer a draft manifest from an existing directory tree by\n  scanning for git repositories and inferring remotes from their origin URLs.",
	"complete": "minirepo complete [--shell bash|zsh|fish|powershell]\n\n  Print a completion script. bash: eval \"$(minirepo complete)\"\n  zsh: works through bashcompinit, see README.",
	"help":     "minirepo help [command...]\n\n  Show the overall help, or help for the given command(s).",
}

func cmdHelp(args []string) {
	if len(args) == 0 {
		// "minirepo help" 是一次**成功的信息请求**,不是"没给命令"的那种用法错误:
		// 总览走 stdout、退出码 0。usage() 是 stderr + exit 2,只该用于真正的用法
		// 错误(README 第一屏就写着 minirepo help,它不能是个失败命令)。
		fmt.Print(usageText())
		return
	}
	for _, a := range args {
		t, ok := cmdHelpText[a]
		if !ok {
			fmt.Fprintf(os.Stderr, "minirepo: error: no help topic for: %s\n", a)
			exitNow(2)
		}
		fmt.Println(t)
		fmt.Println()
	}
}

func usageText() string {
	return fmt.Sprintf(`minirepo %s - a minimal multi-repo workspace manager (repo-compatible manifest subset)

Usage:
  minirepo init -u <manifests repo URL or local path> -m <manifest.xml> [-b branch]
  minirepo sync  [-j N] [--force] [--dry-run] [project...]
  minirepo list  [--paths] [-a]
  minirepo status [project...]
  minirepo forall -c <command> [project...]
  minirepo manifest [--json]
  minirepo freeze -o <snapshot.xml>
  minirepo gen   [-o FILE] <dir>
  minirepo complete [--shell bash|zsh|fish|powershell]
  minirepo help [command...]
  minirepo version

Manifest: a declared subset of repo's XML. See CONTRACT.md; unknown
features in a manifest are warned about once, never silently dropped.

Typical flow:
  minirepo init -u http://host/team/manifests.git -m product.xml
  minirepo sync
  minirepo status
  minirepo forall -c "git status -s"
  minirepo freeze -o release-v1.xml

Install shell completion (bash):
  eval "$(minirepo complete)"
`, versionString())
}

func usage() {
	fmt.Fprint(os.Stderr, usageText())
	exitNow(2)
}
