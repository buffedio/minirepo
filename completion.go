package main

// completion.go —— shell 补全。bash 是权威版本(zsh 经 bashcompinit 复用同一份,
// 语义已核对过 zsh 源码的映射;fish 原生;powershell 静态简版)。
//
// 两个刻意为之的点(别"好心"改回去):
//   - 只注册 `-o default`,不注册 `-o filenames`:项目名本身常是目录,
//     filenames 会把 `sync kernel` 补成 `kernel/`,而过滤是精确匹配,直接报错。
//     目录斜杠由脚本自己加(__mr_slash_dirs)。
//   - forall -c 不补命令名:compgen -c 每次遍历整个 PATH(30-60ms),而
//     -c 'git status -s' 的值 99% 带引号,待补词带着引号永远匹配不上。

import (
	"flag"
	"fmt"
	"strings"
)

var shells = []string{"bash", "zsh", "fish", "powershell"}

const bashCompletionTmpl = `# bash completion for minirepo (eval "$(minirepo complete)")
__mr_find_root() {
    # nearest directory holding .minirepo/ (same rule as the tool)
    local d="$PWD"
    while [ -n "$d" ] && [ "$d" != "/" ]; do
        if [ -d "$d/.minirepo" ]; then printf '%s\n' "$d"; return 0; fi
        d="${d%/*}"
    done
    return 1
}
__mr_exe() {
    # the binary as the user actually spelled it on this command line
    local exe="${COMP_WORDS[0]:-minirepo}"
    case "$exe" in
        */*) [ -x "$exe" ] && printf '%s\n' "$exe" && return 0 ;;
        *)   exe="$(command -v "$exe" 2>/dev/null)" && printf '%s\n' "$exe" && return 0 ;;
    esac
    return 1
}
__mr_projects() {
    # project paths of this workspace; empty outside one, so the -o default
    # fallback takes over. NB compgen -W word-splits: a project path with a
    # space would be split (does not happen in repo/git manifests)
    local root exe
    root="$(__mr_find_root)" || return 0
    exe="$(__mr_exe)" || return 0
    ( cd "$root" && "$exe" list --paths 2>/dev/null )
}
__mr_slash_dirs() {
    # append '/' to directory candidates (stands in for -o filenames)
    local c out=()
    for c in "$@"; do [ -d "$c" ] && out+=("$c/") || out+=("$c"); done
    printf '%s\n' "${out[@]}"
}
__mr_manifest_branches() {
    # branch names of the manifests checkout (empty outside a workspace)
    local root repo
    root="$(__mr_find_root)" || return 0
    repo="$root/.minirepo/manifests"
    [ -d "$repo/.git" ] || return 0
    {
        git -C "$repo" for-each-ref --format='%(refname:short)' refs/heads
        git -C "$repo" for-each-ref --format='%(refname:short)' refs/remotes |
            sed -e 's#^origin/##' -e '/^HEAD$/d'
    } | sort -u
}
_minirepo() {
    local cur prev sub="" i
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    for ((i = 1; i < COMP_CWORD; i++)); do
        case "${COMP_WORDS[$i]}" in
            init|sync|list|status|forall|manifest|freeze|gen|complete|help)
                sub="${COMP_WORDS[$i]}"; break ;;
        esac
    done
    local SUBS="init sync list status forall manifest freeze gen complete help"
    local CMDS="init sync list status forall manifest freeze gen complete"

    # 1) prev is a value-taking option: complete its VALUE, not $cur
    case "$prev" in
        -u|--url|-m|--manifest)
            return 1 ;;                       # URL / path inside the manifests repo: -o default file fallback
        -o|--output)
            return 1 ;;                       # output file: same
        -b|--branch)
            COMPREPLY=( $(compgen -W "$(__mr_manifest_branches)" -- "$cur") )
            [ ${#COMPREPLY[@]} -gt 0 ] && return 0
            return 1 ;;                       # outside a workspace: fall back to file names
        -j|--jobs)
            COMPREPLY=( $(compgen -W "1 2 4 8 12 16" -- "$cur") ); return 0 ;;
        -c|--command)
            return 1 ;;                       # command names deliberately skipped, see the note up top
        --shell|-s)
            COMPREPLY=( $(compgen -W "SHELLS" -- "$cur") ); return 0 ;;
    esac

    # 2) current word starts with '-': complete this subcommand's options
    case "$cur" in
        -*)
            case "$sub" in
                init)     COMPREPLY=( $(compgen -W "-u --url -m --manifest -b --branch --force -h --help" -- "$cur") ) ;;
                sync)     COMPREPLY=( $(compgen -W "-j --jobs --force --no-fetch --dry-run --update-manifests -h --help" -- "$cur") ) ;;
                list)     COMPREPLY=( $(compgen -W "-a --all --paths -h --help" -- "$cur") ) ;;
                status)   COMPREPLY=( $(compgen -W "-h --help" -- "$cur") ) ;;
                forall)   COMPREPLY=( $(compgen -W "-c --command -j --jobs -h --help" -- "$cur") ) ;;
                manifest) COMPREPLY=( $(compgen -W "--json -h --help" -- "$cur") ) ;;
                freeze)   COMPREPLY=( $(compgen -W "-o --output -h --help" -- "$cur") ) ;;
                gen)      COMPREPLY=( $(compgen -W "-o --output -h --help" -- "$cur") ) ;;
                complete) COMPREPLY=( $(compgen -W "-s --shell -h --help" -- "$cur") ) ;;
                help)     : ;;
                *)        COMPREPLY=( $(compgen -W "--help --version" -- "$cur") ) ;;
            esac
            return 0 ;;
    esac

    # 3) positional arguments
    case "$sub" in
        sync|status|forall|list)
            COMPREPLY=( $(compgen -W "$(__mr_projects)" -- "$cur") )
            [ ${#COMPREPLY[@]} -gt 0 ] && return 0
            return 1 ;;                       # no project matched: still allow path completion
        gen)
            local dirs
            dirs=$(compgen -d -- "$cur")
            COMPREPLY=( $(__mr_slash_dirs $dirs) ) ;;
        help)
            COMPREPLY=( $(compgen -W "$CMDS" -- "$cur") ) ;;
        "")
            COMPREPLY=( $(compgen -W "$SUBS" -- "$cur") ) ;;
        *)
            return 1 ;;                       # init/manifest/freeze/complete take no positional arguments
    esac
}
complete -o default -F _minirepo minirepo
#  (a) with no candidates the function returns, readline falls back to
#      file-name completion -- this is what makes '~/x<TAB>' work at all
#  (b) no -o filenames: a project name is often also a directory, so it
#      would complete to 'kernel/' and then fail the exact-match filter
`

const zshCompletionTmpl = `#compdef minirepo
# zsh: reuses the bash script (bashcompinit maps -o default and compgen -W/-d/-f/-c)
if ! command -v compdef >/dev/null 2>&1; then
    autoload -Uz compinit bashcompinit
    compinit
    bashcompinit
fi
eval "$(minirepo complete --shell bash)"
`

const fishCompletionTmpl = `# fish completion for minirepo -- run:  eval (minirepo complete --shell fish | source)
# or save as ~/.config/fish/completions/minirepo.fish (then drop the eval line)
function __minirepo_projects
    minirepo list --paths 2>/dev/null
end
function __minirepo_branches
    set -l root (pwd)
    while test "$root" != /
        if test -d "$root/.minirepo/manifests/.git"
            git -C "$root/.minirepo/manifests" for-each-ref --format='%(refname:short)' refs/heads refs/remotes 2>/dev/null | string replace -r '^origin/' '' | grep -v '^HEAD$'
            return
        end
        set root (dirname "$root")
    end
end
for c in minirepo minirepo.py
    complete -c $c -e
    complete -c $c -s h -l help -d 'show help'
    complete -c $c -n '__fish_use_subcommand' -a 'init sync list status forall manifest freeze gen complete help'
    complete -c $c -n '__fish_seen_subcommand_from init' -l url -r -d 'manifests repo URL or path'
    complete -c $c -n '__fish_seen_subcommand_from init' -l manifest -r -d 'manifest file inside that repo'
    complete -c $c -n '__fish_seen_subcommand_from init' -l branch -r -a '(__minirepo_branches)'
    complete -c $c -n '__fish_seen_subcommand_from init' -l force -d 're-initialize'
    complete -c $c -n '__fish_seen_subcommand_from sync' -s j -l jobs -r -d 'parallel jobs'
    complete -c $c -n '__fish_seen_subcommand_from sync' -l force -d 'overwrite local changes'
    complete -c $c -n '__fish_seen_subcommand_from sync' -l no-fetch -d 'offline checkout'
    complete -c $c -n '__fish_seen_subcommand_from sync' -l dry-run -d 'predict only'
    complete -c $c -n '__fish_seen_subcommand_from sync' -l update-manifests -d 'update manifests first'
    complete -c $c -n '__fish_seen_subcommand_from sync; and not __fish_is_nth_token 1' -a '(__minirepo_projects)' -d 'project'
    complete -c $c -n '__fish_seen_subcommand_from list' -l all -s a -d 'show URLs'
    complete -c $c -n '__fish_seen_subcommand_from list' -l paths -d 'paths only'
    complete -c $c -n '__fish_seen_subcommand_from list' -a '(__minirepo_projects)' -d 'project'
    complete -c $c -n '__fish_seen_subcommand_from forall' -s c -l command -r -d 'command to run'
    complete -c $c -n '__fish_seen_subcommand_from forall' -s j -l jobs -r
    complete -c $c -n '__fish_seen_subcommand_from forall' -a '(__minirepo_projects)' -d 'project'
    complete -c $c -n '__fish_seen_subcommand_from freeze' -l output -s o -r -d 'output file'
    complete -c $c -n '__fish_seen_subcommand_from gen' -l output -s o -r -d 'output file'
    complete -c $c -n '__fish_seen_subcommand_from gen' -a '(__fish_complete_directories)' -d 'directory'
    complete -c $c -n '__fish_seen_subcommand_from manifest' -l json -d 'machine readable'
    complete -c $c -n '__fish_seen_subcommand_from complete' -l shell -r -a 'bash zsh fish powershell'
end
`

const powershellCompletionTmpl = `# minirepo powershell completion; add to $PROFILE
Register-ArgumentCompleter -Native -CommandName minirepo -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $commands = 'init','sync','list','status','forall','manifest','freeze','gen','complete','help','version'
    if ($commandAst.CommandElements.Count -le 2) {
        $commands | Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_) }
        return
    }
    $prev = $commandAst.CommandElements[-2].ToString()
    if ($prev -eq 'complete') {
        'bash','zsh','fish','powershell' | Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_) }
    } elseif ($wordToComplete -like '-*') {
        '-j','--jobs','-b','--branch','-c','--command','-o','--output','-m','--manifest','-u','--url','--force','--dry-run','--no-fetch' |
            Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_) }
    } else {
        minirepo list --paths 2>$null | Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_) }
    }
}
`

func completeFlags() *flag.FlagSet {
	fs := newFlagSet("complete")
	str(fs, "s", "shell", "bash", "shell flavor: "+strings.Join(shells, "/"))
	return fs
}

// completionScript 按 shell 出脚本。SHELLS 占位符在这里被真值替换,
// 所以补全里宣传的 shell 列表永远等于 shells 变量(唯一真值来源)。
func completionScript(shell string) (string, bool) {
	switch shell {
	case "bash":
		return strings.ReplaceAll(bashCompletionTmpl, "SHELLS", strings.Join(shells, " ")), true
	case "zsh":
		return zshCompletionTmpl, true
	case "fish":
		return fishCompletionTmpl, true
	case "powershell":
		return powershellCompletionTmpl, true
	}
	return "", false
}

func mustCompletion(shell string) string {
	script, ok := completionScript(shell)
	if !ok {
		die("unsupported shell %q (supported: %s)", shell, strings.Join(shells, ", "))
	}
	return script
}

func cmdComplete(args []string) {
	fs := completeFlags()
	parse(fs, args) // 只解析一次:parse 里含 -j4/-c=... 这类 GNU 连写的归一化
	requireNoArgs(fs)
	// 走 completionScript 这一条路:否则"支持的 shell"就有两份清单,加一个漏一个
	// (本项目返工次数最多的正是这类漂移)。
	fmt.Print(mustCompletion(strOf(fs, "shell")))
}
