# minirepo

精简的多仓库工作区管理工具 —— Google `repo` 的日常子集，**单个静态二进制**，
无 Python / 无 CGO / 无配置语言要学（直接用 repo 的 XML 清单）。

一份清单描述「哪些 git 仓库和压缩包、放在哪个目录、钉什么版本」，
一条 `minirepo sync` 把整条产品线的工作区拉到一致状态。

## 为什么不用 repo / submodule

| | repo | submodule | minirepo |
|---|---|---|---|
| 依赖 | Python 3.6+ + launcher | git 内置 | 无，拷二进制即用 |
| Windows | 差（symlink、pathsep 到处补） | 一般 | 原生（交叉编译四平台） |
| 清单格式 | XML（1.x/2.x 两代） | 散在各 gitlink | repo XML 的**已声明子集** |
| 命令面 | 30+ 子命令 | — | 10 个，覆盖日常 |
| review/upload | 内置 Gerrit 流程 | — | 不搬，那是 git push 的事 |
| 单次调用 | ~140 ms | — | ~1.6 ms |
| 12000 项目清单 | — | — | `status` 90 ms（线性，不是 O(n²)） |

单次调用延迟在 9 项目的真实 SDK 工作区上测；规模那一行是合成的 12000 项目清单
（无一个仓库存在，全部走 MISSING 探测）实测：`list` 63 ms、`sync --dry-run` 92 ms
—— 项目数 ×4 时耗时 ×3.7~3.8，即线性。清单解析另有 256 个文件的 include 预算，
防的是指数展开而不是规模。

## 安装

```bash
go install github.com/buffedio/minirepo@latest   # 有 Go 工具链：装到 $(go env GOPATH)/bin
git clone https://github.com/buffedio/minirepo.git && cd minirepo
make build            # 产出 ./minirepo（CGO_ENABLED=0，纯静态）
sudo cp minirepo /usr/local/bin/
make release          # 交叉编译 linux/{amd64,arm64} windows/amd64 darwin/{amd64,arm64} 到 dist/
# 国内加速镜像（与主仓同一棵树）：git clone https://gitee.com/buffed/minirepo.git
```

没有 Go 工具链：拿发布方 `make release` 产出的二进制放进 PATH（`dist/` 不入库）。
验证：`minirepo version`。

## 快速上手

```bash
# 1) 初始化：唯一的 bootstrap，只 clone 清单仓
mkdir my-ws && cd my-ws
minirepo init -u https://git.example.com/team/manifests.git -m product.xml
#    本地路径也行： -u /srv/manifests -m default.xml [-b branch] [--force]

# 2) 同步
minirepo sync                      # clone 缺失的 + fetch + 对齐检出
minirepo sync --dry-run            # 预演（连"会拒绝哪些项目"都如实报）
minirepo sync -j 8 ToolChain       # 只同步指定项目（名字或路径，可带尾斜杠）

# 3) 看状态 / 批量跑命令
minirepo status
minirepo list -a                     # 解析后的项目与 URL(凭据已掩码)
minirepo forall -c "git status -sb"
minirepo manifest --json

# 手上有现成的目录树(没有清单)?逆推一份草稿出来
minirepo gen -o draft.xml ~/existing/sdk/tree

# 4) 发布：把当前所有 HEAD 钉成快照
minirepo freeze -o release-v1.xml
#    别人重建这个快照：minirepo init -u <同一清单仓> -m release-v1.xml --force && minirepo sync
```

清单不用改格式：repo 的 `default.xml` 只要落在下面的子集里就能直接用。任何
含多 remote + tag 修订 + 归档包的 SDK 工作区，都是现成的验证样本。

## 清单：支持的子集

```xml
<manifest>
  <remote name="oe" fetch="https://git.example.com/openembedded/" />
  <remote name="pub" fetch="../mirror/" />            <!-- 相对清单仓 URL -->
  <default remote="oe" revision="master" sync-c="true" />

  <project name="app-demo" path="apps/demo" />
  <project name="toolchain" path="prebuilt/toolchain"
           remote="pub" revision="v2.3" />            <!-- tag / 分支 / 40 位 SHA -->
  <project name="mirror-only" path="other" url="https://backup.example.com/other.git" />
  <project name="docs" path="docs">
    <copyfile src="Makefile" dest="build.mk" />
  </project>

  <include name="common/base.xml" />
  <remove-project name="app-demo" />                 <!-- 裁剪继承来的项目 -->

  <archive url="https://www.lua.org/ftp/lua-5.1.5.tar.gz"
           path="external/lua" sha256="26…" strip="1" />   <!-- minirepo 扩展 -->
</manifest>
```

支持：`<remote>` `<default>` `<project>`（`name path remote revision url sync-c`）
`<copyfile>` `<include>` `<remove-project>` `<archive>`。

**其余一律不实现，但会告诉你**：清单里出现 `groups`、`linkfile`、`extend-project`、
`<default sync-j>` 之类时，第一次解析会打印一次
`minirepo: warning: unsupported manifest features ignored: ...`，然后忽略。
边界可见比静默失真重要 —— 完整规则（以及为什么）见 **[CONTRACT.md](CONTRACT.md)**，
那份文件同时是这个工具的验收标准。

## 命令一览

| 命令 | 作用 |
|---|---|
| `init -u <清单仓> -m <文件> [-b 分支] [--force]` | 唯一的 bootstrap，clone 清单仓并记录引用 |
| `sync [-j N] [--force] [--no-fetch] [--dry-run] [--update-manifests] [项目…]` | 让工作区等于清单 |
| `list [--paths] [-a] [项目…]` | 解析后的项目表；`--paths` 给脚本用 |
| `status [项目…]` | 每个项目的分支 / clean-dirty / HEAD / 与清单的偏差 |
| `forall -c <命令> [-j N] [项目…]` | 在每个项目里跑命令，失败才带 `=== path ===` |
| `manifest [--json]` | 当前生效的清单、来源、修订 |
| `freeze -o <快照.xml>` | 把全部 HEAD 钉成独立快照清单 |
| `gen [-o FILE] <目录>` | 从已有目录树逆推清单草稿（扫 git 仓 + 猜 remote） |
| `complete [--shell bash\|zsh\|fish\|powershell]` | 打印补全脚本 |
| `help [命令…]` | 总览或指定命令的用法 |

```bash
minirepo help            # 总览
minirepo help sync list  # 指定命令的完整用法（可一次看多个）
```

`minirepo version` 打印构建时注入的 git 版本。所有命令（除 `init`/`gen`）可在
工作区任意子目录执行。

## sync 的语义（从 repo 过来最容易踩的三条）

1. **`sync` 不做 merge / rebase**。它只 fetch + 对齐，冲突时**拒绝并告诉你怎么
   办**，绝不自动 rebase 你的提交（repo 会）。
2. **`--force` 不是「同步」，是「放弃本地」**：它 `reset --hard`，未提交的改动
   会没。想同时要别人的代码和自己的改动，用标准 git：

   ```bash
   cd <项目>
   git rebase origin/master        # 已提交的改动
   # 或
   git stash && minirepo sync && git stash pop   # 还没提交
   ```
3. **`sync-c="true"` 才给你可提交的本地分支**。没写它时是分离头指针，且分支名
   跟随 fetch 后的 `origin/<branch>`（不是本地同名分支——那个永远停在 clone 那
   一刻）。`freeze` 出的快照全是 SHA，天然 detached，属正常。

## Shell 补全

```bash
# bash / 装了 bashcompinit 的 zsh
eval "$(minirepo complete)"                  # 写进 ~/.bashrc
# zsh 也可以直接：eval "$(minirepo complete --shell zsh)"（它会自己引导 bashcompinit）
# fish：  minirepo complete --shell fish > ~/.config/fish/completions/minirepo.fish
# pwsh：  Add-Content $PROFILE (minirepo complete --shell powershell)
```

补的是什么：子命令、每个子命令的真实选项、`sync/status/list/forall` 的**项目
路径**（现读 `minirepo list --paths`）、`init -b` 的**清单仓分支**、`-j` 的常用
并发数、`gen <目录>`（带尾斜杠）、`complete --shell` 的可选值；函数给不出候选时
回退 readline 的文件名补全，所以 `~/xxx<TAB>`、`-u /tm<TAB>` 这类路径在任何位置
都能补。两个刻意为之的例外（理由写在 `completion.go` 顶部，别"好心"加回来）：
不用 `-o filenames`（会把项目名补成 `kernel/` 而精确匹配会失败）、`forall -c` 不
补命令名（每次 TAB 遍历整个 PATH，而带引号的命令串永远匹配不上）。

## 迁移

**从 Google repo**：清单直接复用（子集内无需改动），`.repo/` 可以留着，minirepo
用的是 `.minirepo/`。日常命令对应关系：`repo init/sync/status/forall/manifest`
同名同义；`repo repo` 特有的 `git checkout -b`、`start`、`upload` 请直接用 git。

## 建议的版本管理约定

- 日常开发：清单里写分支名 + `sync-c="true"`，项目里就是普通 git 分支，随便
  commit / pull / push。
- 出 release：`minirepo freeze -o release-<ver>.xml` 提交进清单仓 —— 从此
  `init -m release-<ver>.xml` 可复现整个产品线，不依赖任何分支还活着。
- 归档包（工具链、SDK 二进制）用 `<archive sha256=...>`，不入库；下载缓存在
  `.minirepo/dl/`，`init --force` 会保留它。

## 环境变量

| 变量 | 作用 |
|---|---|
| `MINIREPO_GIT_TIMEOUT` | 单次 git 操作超时秒数，默认 600（大仓 clone 慢时调大） |
| `NO_COLOR` | 禁用彩色输出（非 TTY 时本来就不上色） |

## 测试

```bash
go test ./...        # 全部离线:本地 git fixture + httptest,不联网、不占固定端口
go test -race ./...  # sync/forall/status 都是并发路径,竞态只能这样抓

# 清单与归档内容都是不可信输入,所以有两个 fuzz target(种子也会随 go test 跑一遍)
go test -run "FuzzParseManifest|FuzzExtract"          # 只跑种子,秒级
go test -fuzz FuzzParseManifest -fuzztime 60s         # 撞解析器
go test -fuzz FuzzExtract     -fuzztime 60s           # 撞解压(zip-slip/符号链接)
```

覆盖方式（三条约定，防止测试退化成橡皮图章）：

- **契约逐节钉住**：`main_test.go`（解析/边界/sync 语义）、`archive_test.go` +
  `archive_http_test.go`（五种格式 gz/bz2/xz/zip/tar × strip 有/无、断点续传、
  坏缓存淘汰、软链逃逸）、`gen_test.go`（草稿能被真工作区 load 并 clone）、
  `compat_test.go`（凭据外泄、丢改动警告、路径冲突、freeze 往返、dry-run 如实、
  forall 输出契约）、`guards_test.go`（补全/帮助/README 与真实 CLI 对拍）。
- **补全是真跑起来的**：`completion_exec_test.go` 把脚本 source 进 bash、喂
  `COMP_WORDS` 断言 `COMPREPLY`（项目名不带尾斜杠、`--shell`/`-b`/`-j` 补的是
  **值**、目录参数带尾斜杠）。没有 bash 时 `t.Skip` 并打印「不是通过」的理由。
- **fuzz 种子必须是合法结构**，否则算力全花在重拼格式上。判据是**执行速率**：种子只
  有 zip 且大多死在 `OpenReader` 时是 938k 次/60s（看着吓人其实空转）；换成 tar /
  tar.gz / tar.xz / zip 四种合法包 + 可变 strip 之后是 53k 次/90s —— 慢了 15 倍，
  因为这次每个输入都真的在解压。写这个 helper 时踩到 `return buf.Bytes(), gw.Close()`
  —— Go 按顺序求值，交出的是**没有尾巴**的压缩流。

- **不可信输入解析器必须自己被 fuzz**：给 xz block header 写的守卫第一版就 panic
  过（`index out of range [12]`）。判据很简单 —— *它会 panic 吗？* 会，就得有自己的
  fuzz target 和"合法文件不许被误杀"的反向用例。

- **新写的测试要用变异验证**：把对应实现逐条改坏，看测试是否变红。目前已按类逐个
  验证过：归档格式/续传/缓存、路径冲突、mask、forall 头、dry-run 判定、丢弃警告、
  根元素、include 环、补全、分发与退出码、下载空闲守卫、解压的三类逃逸防线 ——
  **全部被抓**。没变红的那几次都是我的断言写空了（漏了 wrong-root 用例；格式矩阵里
  没有顶层目录等于没测 strip；越界 seed 没给合法 remote，先死在"缺 remote"，路径防线
  根本没被走到）。规则是**补用例，不改实现**。
- **拒绝类用例必须断言"因什么被拒"**：`seedWant` 表把每条坏清单对应到它应当触发的
  那条防线。只断言"报错就算过"的话，一个不相干的早退就能让整条测试空转（这里真发生
  过）。fuzz 输入没有预期，所以走另一档语义：可以被拒，但消息必须说清，且被接受时
  必须满足不变量（path 在界内、URL 过白名单、URL 不以 `-` 开头、revision 非空）。
  这一步真查出过两次：一次是我漏了 wrong-root 用例，一次是格式矩阵里没有顶层目录、
  等于没测 strip。


## 中断之后（Ctrl-C / kill）

`sync` 可以随便打断,重跑就行:克隆是原子的(先写 `.git` 再检出),归档下载留 `.part`
续传,解压写临时目录、成功才换名。被打断时**唯一**需要你动手的情况是 git 留下一把
`index.lock`(通常是 `kill -9` 杀了 minirepo 而 git 子进程还在写)——工具会拒绝并说出
是哪个文件,把它删掉再 sync 即可。它不会替你删:锁可能属于一个还活着的 git,自动删
就是在损坏仓库。解压的临时目录 `<dest>.minirepo-tmp` 用固定名并在下次开头清掉,所以
中断不会积累垃圾;解压失败也不留半成品。你自己的工作目录内容它也从不删:目录存在但不是 git 仓库时直接拒绝。

## 版本串

官方构建**只有 Makefile**（`make build` / `make release`，注入 `git describe`）。
裸 `go build` 会退化成短 hash（Go 只嵌 revision、不认识 tag），仅供本地调试。

| 场景 | `minirepo version` |
|---|---|
| 正好在 tag 上、树干净 | `v0.10.0` |
| tag 之后有 N 个提交 | `v0.10.0-N-g<hash>` |
| 工作区有未提交改动 | 尾巴加 `-dirty` |

tag 一旦有人消费就不可变；修正走 `v0.10.1`，破坏性变更走 `v0.11.0`/`1.0.0`。
被 force-move 过的 tag，其他已有克隆必须 `git fetch --tags --force` 才会更新——
否则 describe 会拿旧 tag 算出误导性的偏移。

## 已知边界

- `forall -c` 走平台 shell：Windows 下是 `cmd.exe`，上面示例里的 POSIX 单引号
  要换成双引号（写错会当场报错，不会静默跑错）。
- 空仓（无 commit）不能被 sync：`gen` 会给它 `revision="HEAD"` 加一条 NOTE，
  请 review 后再用。
- 清单里 `groups` / `linkfile` 等特性不实现（见上），只警告不模拟。
- 不做并发 `sync` 互斥：同一工作区请串行执行（repo 也没有）。

## License

MIT，见 [LICENSE](LICENSE)。
