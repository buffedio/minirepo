# minirepo 语义契约

这份文件是**验收标准**，不是使用说明。它描述「minirepo 对一份清单承诺了什么」，
**本文件是唯一规范**，与实现语言无关：这里的每一条都由本仓库的
`main_test.go` / `archive_test.go` / `gen_test.go` / `guards_test.go` 钉住。
改行为必须先改本文件，再改代码，否则测试会红。

历史：这些规则来自一个更早的内部实现，用真实 SDK 踩出来后逐条移植、一条没丢；
那个实现不再是本项目的依据，也不再被引用。

改任何一条规则之前先改这里，再改测试，最后改代码。

## 0. 三条判据

每个想加的东西都过这三关，过不了就不进代码库：

1. **不新增表面**：不加命令、不加参数、不加状态文件、不加新的语义分支。
2. **不猜**：宁可报错退出，也不静默兜底。`revision` 缺失就报错，不会替你填
   `master`；清单里有不支持的特性就警告，不会当它不存在。
3. **不让已声明的子集失真**：文档承诺「repo 清单的某个子集可直接用」，那这个
   **范围内的错**必须修；范围外的可以不做，但**必须响**。

## 1. 清单：一个「已声明」的 XML 子集

支持：

| 元素 | 属性 |
|---|---|
| `<remote>` | `name` `fetch` |
| `<default>` | `remote` `revision` `sync-c` |
| `<project>` | `name` `path` `remote` `revision` `url` `sync-c` + 子元素 `<copyfile src dest>` |
| `<include name>` | 递归合并 |
| `<remove-project name>` | 裁剪 |
| `<archive url path sha256 strip format>` | minirepo 扩展（非 repo 特性） |

规则：

- **与 Google repo 的互操作已实测**（repo v2.65）：两份真实生产清单在
  `repo init` + `repo manifest` 下与源文件**逐项等价**（revision 等于
  `<default>` 时被正常化省略，不是丢失）。清单只应使用两个工具共同支持的
  元素/属性；`groups` / `clone-depth` / `linkfile` 等 repo 支持而本工具不支持的，
  本工具会警告并忽略 —— 那时两边行为分叉，所以**改清单后要跑一次 `list` 看
  stderr 有没有警告**。
- **未知元素/未知属性**：一次性警告 `unsupported manifest features ignored:
  <linkfile>, project@groups, default@sync-j ...`，然后忽略。警告走 stderr，
  stdout 保持机器可读。**不许静默丢弃**——用户必须能看见边界在哪。
- **`<default>` 与 `<include>` 都是文档顺序**：后出现的同名项覆盖先出现的；
  父文件写过的（包括显式 `sync-c="false"`）子清单**不得**覆盖。
- **`<remove-project>` 按文档顺序生效**：删掉它之前定义的项目；之后重新声明同名
  项目 = 换路径/换 revision 的正规写法。
- **`revision` 缺失 = 报错**（`<project>` 和 `<default>` 都没给才算缺失）。
- **`sync-c` 继承**：`<default sync-c="true">` 对所有没写该属性的项目生效；
  项目显式 `false` 覆盖。
- **`<copyfile>` 只支持 `<project>` 内的形式**；repo 的老式顶层 `<copyfile>`
  进警告列表，不实现。
- **根元素必须是 `<manifest>`**：Go 的 XML decoder 对"整篇没有元素"的文档不报错，
  未知根元素也只会被当成未支持特性跳过 —— 于是随手一个文本文件就能"成功"初始化出
  一个 0 项目工作区。现在明确失败（`not a repo manifest`）。
- **`<include>` 成环 = 报错**（`circular <include>`），不是栈溢出。
- 清单合法但什么都不声明时 `init` **警告并继续**（不 die）：`init` 也是重新指向清单
  的手段，取消这个能力换来的收益不值，何况计数行本来就写着 0。

## 2. URL 解析

- **相对 `fetch` / `url` 按 manifests 仓的 URL 解析**（repo 规则：`..` 去掉清单
  仓 URL 的一段），**与命令执行目录无关**：同一工作区里，工作区根和任意子目录
  跑 `sync` 必须得到同一批 URL。
- `<project url="...">` **覆盖** `fetch + name` 拼出的地址；`freeze` 必须把这个
  属性原样带回快照，否则快照不可用。
- 拼完仍是相对路径 → 报错（不许拿相对 URL 去 clone，那等于按 CWD 猜）。
- `file://` 的本地路径要过 percent 解码。

## 3. 工作区是一个边界，不是提示

以下四项**绝对路径和 `..` 一律拒绝**（清单可能来自第三方 SDK，必须当不可信输入）：

`<project path>` · `<copyfile src>` · `<copyfile dest>` · `<archive path>`

`<include name>` 是唯一例外：**清单仓内**可以用 `..`（子目录清单引用同仓文件是
合法用法），但不能出仓（比较 realpath，防符号链接）。

嵌套项目（同时声明 `a` 与 `a/b`，Buildroot 就这么干）是**允许**的；一个路径只能
有**一个主人**：项目之间、项目与归档之间、归档之间的重复声明一律拒绝。报错必须
**点名双方**（`path "x" is claimed by both project a and archive <url>`）——早先
归档之间冲突时消息把对方说成 `project <路径>`，看着像工具搞不清自己声明了什么。

嵌套的 clone 必须**按深度分波**（父波全部结束才开子波）。把它们丢进同一个线程池
会让子项目抢跑、父项目 clone 报「目录非空」，并留下一个既非空也不是 git 仓的
废墟，从此每次 sync 永久失败。扁平清单只有一波，并行度不受影响。

## 4. 清单是不可信输入（安全）

- **传输白名单**：只接受 `http(s)` / `git` / `ssh` / `file` 和 scp 式
  `host:path`、本地路径。`ext::sh -c ...`、`fd::`、任意 `<helper>::` 一律拒绝
  —— 现代 git 默认禁 `ext`，但不能把安全性押在 git 版本上。以 `-` 开头的 URL
  也拒绝（会被 git 当选项）。clone 一律 `-- <url>`。
- **凭据不外泄**：`.minirepo/config.json` 以 **0600** 写；**所有显示路径**
  （`manifest`、`manifest --json`、`list -a`、`init` 预览、归档行）过 `maskURL`。
  注意相对 `fetch` 会把清单仓 URL 里的凭据带进**每一条**项目 URL，只屏蔽顶层
  不算修完。只有写进清单文件（`freeze`）时才保留原值。
- 屏蔽的是**整个 userinfo**，不只是口令：`https://<token>@host` 这种把令牌放在
  username 位置的写法很常见，保留用户名（`bot:***@`）就等于可能还泄着。
- 归档解压拒绝越界成员（`../`、绝对路径）、拒绝符号链接目标逃逸。**硬链接 / fifo /
  字符设备等成员类型不落地**（`os.Link` 可以指向解压目录之外的任意已有文件，路径白名单
  挡不住它；设备节点在构建机上是提权面），但必须**计入跳过并警告** —— 静默丢掉一半内容
  比多一行警告危险。zip 的符号链接条目写成普通文件（不建链接 = 无逃逸面），tar 才建
  真链接：这个不一致是刻意的，两边各有一条测试钉住。
- 成员落地的检查有**两层独立的**判据：词法（`../` 前缀、绝对路径）与 realpath
  （`escapesRoot`）。去掉任一层，另一层仍挡得住 —— 做变异验证时不要把"少改一层没变红"
  当成覆盖缺口，正确做法是把整条防线拆掉看是否变红（已验证：拆整条 → 红）。
- 写穿符号链接的两个方向都要挡，而且**判据不同**：
  链接指向界外 → `linkTargetInside` 拒创建；链接指向**界内**、之后有成员写穿它去覆盖
  真文件 → `throughSymlink` 跳过。后者不会触发任何"逃逸"断言（覆盖发生在界内），
  只能用**内容**判据看出来（`pkg/real.txt` 必须还是 `good` 而不是 `EVIL`）。
- 被我们**拒绝创建**的链接，其路径下的后续成员也不许落地（`blockedAncestor`）：否则
  同一个包会因为"链接被跳过"而多出一个同名目录，布局取决于工具的实现 —— 一致性缺陷。
- 一条归档的 URL 必须与项目的 URL 过**同一条**传输白名单（曾因只包了项目一侧而被
  fuzz 抓到：`<archive url="A0:">` 原样返回）。
- 解压的失败语义是「恶意成员跳过、其余保留、整体成功」，不是「整包报错」。

## 5. sync 的语义：对齐清单，仅此而已

`sync` = clone 缺失的 + fetch + 把检出对齐到清单的 revision。**永不自动
merge、永不自动 rebase**（这点和 Android repo 不同：repo 会 rebase 你的本地
提交）。要么 fast-forward，要么拒绝并告诉你下一步。

| 状态 | 行为 |
|---|---|
| 干净、落后 | ff / 检出到目标 |
| 有未提交改动 | `local changes; use --force to overwrite`，**不碰工作树** |
| 分支与远端分岐 | `branch 'x' diverged from origin/x; commit/push or use --force` |
| 本地分支有未推送提交（sync-c） | 拒绝，不许 `checkout -B` 覆盖 |
| `--force` | `reset --hard`（此时未提交改动会没，这是它的定义） |
| `--no-fetch` | 不碰网络，只用本地对象对齐 |

检出模式：

- **分支 + `sync-c`** → 本地跟踪分支（`checkout -B` / `merge --ff-only`），
  可以直接 commit/pull。
- **sha / tag / 无 `sync-c`** → 分离头指针。**分支名要跟随 fetch 后的远端 tip
  （`origin/<branch>`）**：`git fetch` 只动 `refs/remotes/*`，本地同名分支永远
  停在 clone 那一刻——用它当目标是「sync 再也不前进但仍报 OK」（0.5.2 修掉）。
- **不用 `--single-branch`**：它会把 `remote.origin.fetch` 收窄到一个分支，于是
  清单里钉在别的分支/tag 上的 revision 永远拉不到。省一次传输换可复现性，不值。
- revision 在上游消失（分支被删、PR 源分支被删）时，报错要**自述原因和下一步**，
  不许只转述 git 的 `reference is not a tree`。
- `origin` 与清单不一致时**纠正并警告**（清单是真相；换源的仓不能继续从旧地址拉）。
- `--update-manifests` 要 reset 清单检出：**先警告再丢**（`discarding local edits
  in .minirepo/manifests (N file(s))`），不然在清单里手改调试的人会静默丢工作。

### git 子进程（§5 补充）

- **一个项目一行**是 `sync` 报告的硬契约。git 失败时不许把它的多行原文塞进报告行:
  实测过的症状是 `[FAIL] s are terminated then try again.` —— git 那句 fatal 的**续行**
  被当成了我们的消息,既读不通,又长得像 minirepo 自己在说话。规则:报告行放
  「动词 + 一句结论」,完整原文加 `  git: ` 前缀走 stderr。
- 结论取**最后一条 `fatal:`/`error:`**(fetch 前面照例是进度噪声),没有就用首行。
  取尾几行是错的:git 的重点在第一行。
- **探测类调用一律 `GIT_TERMINAL_PROMPT=0`**。git 要凭据时是直接开 `/dev/tty` 的,
  不受 stdin 重定向影响 —— 私有 https 远端会让一次 `minirepo status` 挂到 git 超时
  (默认 600s)才失败。只有 `clone`/`fetch` 且 stdout 还在终端上时才允许问人。
- 大仓库 `clone` **不设总时长**:38MB 与 3GB 不该共用一个数,要防的是挂死而不是慢,
  那由上一条与下载侧的空闲守卫负责。
- **绝不代删锁文件**。`.git/index.lock` 可能属于一个还活着的 git 子进程(`kill -9
  minirepo` 只杀父进程),自动删就是损坏仓库。只拒绝 + 把是哪个文件说出来。
- 锁存在时只读命令(`status`/`list`/`manifest`)必须照常可用。

## 6. 预览必须诚实

`--dry-run` 的判定必须等于真实 sync 的判定，包括**会拒绝的项目**：脏项目报
`would refuse: local changes` 并计入失败（exit 1），这样
`minirepo sync --dry-run && minirepo sync` 才是安全的串联。同理 dry-run 要带上
copyfile 数量。任何「预演说会做、真做时拒绝」都是 bug。

`freeze` 的快照必须能被**新工作区** `init -m snap.xml && sync` 重建出一模一样的
HEAD；做不到就是快照丢了信息（`url=`、`remote` 都属于会被丢的东西）。

## 7. 归档（`<archive>`）

- 缓存键必须含 URL 摘要（`sha256(url)[:8]-basename`）：只用 basename 会让两个
  同名不同址的包互删缓存、每次 sync 全量重下。
- 断点续传：`.part` + HTTP Range；校验用 sha256；未钉 sha256 要警告
  「快照不可复现」。
- **下载只防「卡住」，不防「慢」**：连接、响应头、以及传输**空闲**三个超时都与
  文件体积无关（3GB 的包本来就要跑几分钟，设总时长只会多一个必须调的参数）。
  `http.DefaultClient` 一个都没有 —— 用它就等于「镜像站挂起 = sync 永久挂起」。
  空闲判定要**每读到数据就刷新时间戳**：只测「卡住会报错」抓不到「误杀慢速但存活
  的下载」，两个方向各有一条测试。
- **xz 文件头里的字典大小是攻击者可控的声明，不是预算**。xz 库解析 block header 后
  做的是 `if dc > config.DictCap { config.DictCap = dc }` —— 声明**抬高**解码器配置。
  实测：把 168 字节 `.xz` 里一个 props 字节改成 40（=4 GiB）并修好 header CRC，一次
  解压就 `make` 出 4 GiB 缓冲；平时靠懒分配只花 16 MiB RSS，但在 `ulimit -v`/VA 受限
  环境下变成 `runtime: out of memory` 的**不可恢复 fatal**：整个 `sync` 一起死、stdout
  上什么报告都没有、也不说哪个包。规则：解压前读第一个 block header 的 LZMA2 props，
  声明 > 256 MiB（最大预设 9e 才 64 MiB，留 4 倍余量）就指名拒绝。
  **覆盖范围必须如实说**：只查第一个 block —— 后面 block 的 header 位置要靠前一个
  block 的压缩长度，而那个字段在 flags 里是可选的、索引又在流尾；所以 `xz -T4` 这类
  多 block 的包只挡住了第一个。这不是完整防线，是把「坏包/简单恶意包」从进程死亡
  变成一句能读的拒绝。
- **守卫自己也是解析不可信输入的代码，比被它守的东西更需要 fuzz**：这条检查的第一版
  就 panic 过（`index out of range [12]`，把 block header 长度按"含流头"算了），而
  block header 里那两个**可选尺寸字段**（`xz -T4` 会写）第一版也没跳，导致真实文件被
  读成 "bad filter record"。现在它有独立的 fuzz target，并且测试必须覆盖
  ①合法预设 `-6`/`-9e` ②`--block-size` 产生的多 block 包（用 `xz --list` 数出的
  block 数当判据，而不是自己再数一遍字节）。
- 换掉 `DefaultTransport` 时必须保留 `Proxy: http.ProxyFromEnvironment`，否则
  `http_proxy` / `no_proxy` **静默失效**，症状是「内网机器连不出去」这种最难查的。
- 解压目录里写 stamp `.minirepo-archive`：**JSON**（`url/path/sha256/strip/format`），
  人能直接 cat，出问题时说得清是哪一项变了。读的时候兼容裸摘要文本（接管旧工作区
  不误判「条目变了」，否则人家得白白 `--force` 重解压）。
- 稳态判断**不碰网络也不重算哈希**：钉了 `sha256` 时拿 stamp 里的摘要直接比；只有
  未钉 `sha256` 才退回去比缓存文件的哈希。否则 `dl/` 被清后，几 GB 的工具链包会为了
  「知道自己不用重解压」而重下一遍。
- `--dry-run` 与 `--no-fetch` 对归档同样生效且判定共用同一套逻辑：stamp 不匹配时
  dry-run 必须说「会被拒、需要 --force」，不许只看 stamp 文件存不存在就报 ok。
- **没有「未指定 strip 就自动剥一层」**：顶层恰好只有一个目录时布局会突然变，
  清单必须自己写 `strip="1"`。
- 压缩包成员是**不可信输入**：绝对路径、`..` 开头的名字、以及**逃逸的软链**
  （`l -> ../../..` 之后再放 `l/pwn` 写穿它）一律跳过并警告。两条边界都要保住：
  指向解压目录内的相对软链（工具链里的 `lib64 -> lib`）必须原样保留；名字以
  `..` 开头的合法文件（`..bashrc`）不许误杀。

## 8. 输出与退出码

- stdout 机器可读（`list --paths`、`manifest --json` 必须可被管道/`json.loads`
  消费）；警告、失败详情一律 stderr。
- `status` 是信息性命令：正常退出 0，有 MISSING 才 1。`sync`/`forall` 有任何
  失败即非 0。
- `forall` 默认**不打**项目名头（对齐 repo：只有失败才在 stderr 打
  `=== path ===`）。**不注入** `$REPO_*`；命令串里出现 `$REPO_` 时警告一次，
  而不是让它静默展开成空。
- 未知子命令 / 未知 help 主题：退出码 2（和 flag 包的用法错误一致）。
- 运行时字符串（帮助、错误、生成的补全脚本）保持 **ASCII**；注释可以中文。

## 9. 明确不做

| 不做的东西 | 为什么 |
|---|---|
| `groups` / `-g` | 多拉仓库是慢不是错；`-g` 是新参数 + 新过滤分支 |
| `linkfile` 实现 | 要碰符号链接语义、freeze 往返、干净性判定 → 功能膨胀；只警告 |
| 注入 `REPO_*` | 那是给 repo 脚本生态当运行期，属于上游包袱 |
| `init -m` 默认 `default.xml` | 必填更 mini 且不猜 |
| 补全结果缓存 | 实测省 ~2ms，代价是可能补出过期项目名（错的比慢糟） |
| `-o filenames` 补全 | 会把项目名补成 `kernel/`，而过滤是精确匹配 → 直接报错 |
| `forall -c` 补命令名 | 每次 TAB 遍历 PATH（30–60ms、4700+ 候选），而 `-c 'git status -s'` 的值带引号时永不匹配 |
| `init --depth` | 性能糖；开了就多一条「浅克隆后拉不到旧 revision」的坑 |
| 并发 sync 加锁 | 要引入状态文件（新表面），repo 自己也没有 |
| 第二份 zsh/fish 原生脚本 | 两处手写必然漂移（`help` 那个 bug 就是这么来的） |

## 10. 性能基线（实测，防止「优化」跑错方向）

| 指标（实测，本机 warm） | 数值 |
|---|---|
| 单次调用（`version`） | 1.6 ms |
| `list` 1000 项目 | 15 ms |
| 20 项目稳态 `sync` | 40 ms |
| 静态二进制 | 6.9 MB |

结论：**清单解析从来不是瓶颈**（1000 项目 15ms 里主要是启动与打印）。补全/同步结果
缓存换来的那点毫秒，抵不上「给出过期答案」的风险——任何「缓存清单解析结果」的提案
都是在优化一个不存在的瓶颈。真要提速，方向是减少**子进程**数量（每次 `git` 调用才是
大头），不是加缓存。
