package main

// archive.go —— 压缩包依赖:下载(缓存+断点续传) → sha256 校验 → 解压。
// 缓存键 = sha256(url)[:8]-basename:两个不同 URL 同名(…/v1/x.tar.gz 与
// …/v2/x.tar.gz)共用一个文件名会互删缓存、反复全量重下 —— 键里必须带 URL。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ulikunitz/xz"
)

// syncArchive 下载→校验→解压到 dst。stamp 记录上次解压的归档内容 sha:
// 一致直接返回(稳态零成本);不一致说明清单条目变了,要求 --force,
// 不静默覆盖。dst 已存在但无 stamp 也不动(不是我们解压的,不背这个锅)。
// extractArchive 按**内容**(不是 URL 后缀)选解码器,解到 dst。
// 单独成一个函数是为了让"嗅探 + 分发"这个小面只有一个入口:测试与 fuzz 打的必须是
// 真实路径,而不是绕过判定的 unzip/untar —— 判错格式的后果是解出垃圾或干脆没警告。
func extractArchive(path, dst string, strip int) error {
	kind, err := sniffArchive(path)
	if err != nil {
		return err
	}
	if kind == "zip" {
		return unzip(path, dst, strip)
	}
	return untar(path, dst, kind, strip)
}

func syncArchive(root string, a Archive, force, noFetch, dryRun bool) (string, error) {
	dest := filepath.Join(root, filepath.FromSlash(a.Path))
	cache := filepath.Join(root, toolDir, downloadsDir, hashName(a.URL))
	stampPath := filepath.Join(dest, archiveStamp)

	// 稳态判断必须**不碰网络也不重算大文件哈希**:stamp 里的摘要等于清单钉的
	// sha256 就足以确定内容没变(只有未钉 sha256 时才退回去比缓存文件的哈希)。
	// 否则 dl/ 缓存一旦被清,几 GB 的工具链包会为了"知道自己不用重解压"而重下一遍。
	cached := func() bool {
		sd, ok := stampDigest(stampPath)
		if !ok {
			return false
		}
		if a.SHA256 != "" {
			return sd == a.SHA256
		}
		if !fileExists(cache) {
			return false
		}
		got, err := fileSHA256(cache)
		return err == nil && got == sd
	}

	if dryRun {
		// --dry-run 的判定与真实路径共用 cached(),不许只"看 stamp 在不在"就报 ok
		if cached() {
			return "ok (cached)", nil
		}
		msg := "would download+extract"
		if fileExists(stampPath) {
			msg = "would re-extract and refuses without --force (stamp != this <archive> entry)"
		}
		if a.SHA256 == "" {
			msg += " (no sha256 pinned!)"
		}
		return msg, nil
	}

	if cached() {
		return "ok (cached)", nil
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		return "", err
	}
	if noFetch && !fileExists(cache) {
		// --no-fetch 的承诺是"不碰网络":缓存没命中就失败,而不是偷偷下载
		return "", errors.New("not in cache and --no-fetch given")
	}
	if err := download(a.URL, cache); err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	got, err := fileSHA256(cache)
	if err != nil {
		return "", err
	}
	if a.SHA256 != "" && got != a.SHA256 {
		// 缓存里的内容对不上清单:可能是同 URL 内容变了,也可能历史遗留。
		os.Remove(cache) // 别让坏缓存活到下一次
		return "", fmt.Errorf("sha256 mismatch for %s: want %s got %s", a.URL, a.SHA256, got)
	}
	if !fileExists(stampPath) && fileExists(dest) {
		return "", fmt.Errorf("target exists but was not extracted by minirepo; remove it or use --force")
	}
	if fileExists(stampPath) && !force {
		return "", fmt.Errorf("extracted by an older <archive> entry (url/sha256/strip/format changed); use --force")
	}

	// 解压到临时目录再原子替换,失败不破坏旧内容
	tmp := dest + ".minirepo-tmp"
	os.RemoveAll(tmp)
	if err := extractArchive(cache, tmp, a.Strip); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	// 没有"未指定 strip 就自动剥一层"的猜测:那样同一个包在不同工具/不同版本下
	// 会给出不同布局(顶层只有一个目录时突然全变),清单必须自己写清 strip="1"。
	if err := replaceDir(tmp, dest); err != nil {
		return "", err
	}
	doc, err := json.MarshalIndent(archiveStampDoc{a.URL, a.Path, got, a.Strip, a.Format}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(stampPath, doc, 0o600); err != nil {
		return "", err
	}
	return "ok", nil
}

func replaceDir(tmp, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// hashName 下载缓存文件名:URL 指纹 + 原名(人还能认出来是什么包)
func hashName(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:8]) + "-" + filepath.Base(strings.TrimSuffix(url, "/"))
}

// download 下载到 dst;dst.part 存在且服务器支持 Range 时断点续传。
// file:// 与本地路径直接复制(相对 URL 已在 resolve 阶段锚定)。
// 下载只防"卡住",不防"慢":一个 3GB 的工具链包本来就要跑几分钟,总时长超时只会
// 变成又一个必须调的参数。所以三个超时都与体积无关:连接、响应头、以及传输中的
// 空闲(对端接了连接却不给数据)。http.DefaultClient 一个都没有,用它就等于把
// "镜像站挂起"变成"minirepo 永久挂起"。
var (
	dialTimeout           = 30 * time.Second
	responseHeaderTimeout = 30 * time.Second
	readIdleTimeout       = 60 * time.Second // 测试里缩短它来验证卡死检测
)

var httpClient = &http.Client{
	Transport: &http.Transport{
		// Proxy 必须显式保留:换成自定义 Transport 后若不写这行,http_proxy /
		// no_proxy 会**静默失效**(DefaultClient 的隐式默认就是这个)。内网机器
		// 走代理出公网的场景全靠它。
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// stalledReader 记录"最后一次读到数据的时刻",看门狗据此掐掉卡死的传输。
type stalledReader struct {
	r    io.Reader
	last *atomic.Int64
}

func (r *stalledReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func download(url, dst string) error {
	if fileExists(dst) {
		return nil
	}
	if !strings.Contains(url, "://") {
		return copyFile(url, dst)
	}
	if strings.HasPrefix(url, "file://") {
		return copyFile(strings.TrimPrefix(url, "file://"), dst)
	}

	part := dst + ".part"
	var req *http.Request
	var err error
	start := int64(0)
	if st, err2 := os.Stat(part); err2 == nil {
		start = st.Size()
	}
	req, err = http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	switch {
	case resp.StatusCode == 206:
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND // 续传
	case resp.StatusCode == 200:
		start = 0 // 服务器不支持 Range:整个重来
	case resp.StatusCode == 416:
		// 续传起点已到文件尾 = part 已经是完整内容
		return os.Rename(part, dst)
	default:
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	last := &atomic.Int64{}
	last.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	// 快照进局部变量:goroutine 读包级变量会与任何后续写入构成数据竞争
	// (`go test -race` 真的报过),而且一次传输的过程中阈值被人改动也不合理。
	idleLimit := readIdleTimeout
	if idleLimit > 0 {
		go func() {
			tick := time.NewTicker(idleLimit / 4)
			defer tick.Stop()
			for {
				select {
				case <-watchdogDone:
					return
				case <-tick.C:
					if idle := time.Since(time.Unix(0, last.Load())); idle > idleLimit {
						// 取消请求:Body 的阻塞读会立刻返回错误。不删 .part —— 已经
						// 下到的字节下次还要用来续传。
						warn("download stalled (%s with no data), giving up: %s", idle.Round(time.Second), url)
						cancel()
						return
					}
				}
			}
		}()
	} // idleLimit <= 0 时不设看门狗(0 会让 Ticker panic)

	if _, err := io.Copy(f, &stalledReader{r: resp.Body, last: last}); err != nil {
		return fmt.Errorf("download failed (partial kept, will resume): %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(part, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sniffArchive 按魔数识别格式(扩展名只用于报错提示,URL 没后缀也能解)
func sniffArchive(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 262)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	supported := ".tar.gz/.tgz/.tar.bz2/.tbz2/.tar.xz/.txz/.tar/.zip"
	switch {
	case len(head) >= 4 && head[0] == 'P' && head[1] == 'K':
		return "zip", nil
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "gzip", nil
	case len(head) >= 3 && string(head[:3]) == "BZh":
		return "bzip2", nil
	case len(head) >= 6 && head[0] == 0xfd && string(head[1:6]) == "7zXZ\x00":
		return "xz", nil
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return "tar", nil
	}
	return "", fmt.Errorf("unrecognized archive format (supported: %s): %s", supported, path)
}

// trimStrip 去掉条目路径前 N 段;返回 "" 表示整个条目被剥掉
// archiveStampDoc 是 dest/.minirepo-archive 的内容。用 JSON(而不是一个裸哈希):
// 旧版写的就是 JSON,接管老工作区必须读得懂;而且出问题时人可以直接 cat 它。
type archiveStampDoc struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Strip  int    `json:"strip"`
	Format string `json:"format"`
}

// stampDigest 兼容两种历史写法:JSON(旧版)与裸摘要文本。
func stampDigest(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "{") {
		var doc archiveStampDoc
		if json.Unmarshal([]byte(s), &doc) != nil {
			return "", false
		}
		return doc.SHA256, doc.SHA256 != ""
	}
	return s, true
}

// escapesRoot 报告 target 是否落在 root 之外(清单与压缩包都是不可信输入)。
// blockedAncestor 报告 name 的任一祖先是否在被拒绝的软链名单里。
func blockedAncestor(blocked []string, name string) bool {
	for _, b := range blocked {
		if strings.HasPrefix(name, b+"/") {
			return true
		}
	}
	return false
}

func escapesRoot(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(rel) {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// throughSymlink 报告 target 的任一祖先目录是不是符号链接。
// 只校验条目名不够:tar 可以先放一个 `l -> ../../..` 的软链,再用普通成员
// `l/pwn` 写穿它 —— 名字干净,落地位置却在外面。
func throughSymlink(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return true
	}
	cur := root
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, d := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, d)
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// linkTargetInside 校验符号链接条目的目标:绝对目标、以及解析后会跑出解压目录的
// 相对目标都拒绝。SDK 工具链里 `lib64 -> lib` 这类仓内相对链接必须保留,
// 所以不能一刀切禁掉软链。
func linkTargetInside(root, name, linkname string) bool {
	if linkname == "" || strings.HasPrefix(linkname, "/") {
		return false
	}
	base := filepath.Join(root, filepath.Dir(filepath.FromSlash(name)))
	return !escapesRoot(root, filepath.Join(base, filepath.FromSlash(linkname)))
}

func trimStrip(name string, strip int) string {
	if strip <= 0 {
		return name
	}
	parts := []string{}
	for _, c := range strings.Split(strings.ReplaceAll(name, "\\", "/"), "/") {
		if c != "" && c != "." {
			parts = append(parts, c)
		}
	}
	if len(parts) <= strip {
		return ""
	}
	return strings.Join(parts[strip:], "/")
}

// untar 解包 tar(可带压缩);防路径穿越:.. 与绝对路径条目跳过并记数
// maxXzDictCap 是能接受的 LZMA2 字典声明上限。xz 最大的预设 9e 也只用 64 MiB,
// 这里留 4 倍余量。
const maxXzDictCap int64 = 256 << 20

// pluralEntry 而不是在运行时字符串里写 "entr(y/ies)":格式串不会自己选单复数,
// 那样用户真会看到 "1 zip entr(y/ies) skipped"。
func pluralEntry(n int) string {
	if n == 1 {
		return "entry"
	}
	return "entries"
}

// xzDictClaim 读出 xz 流**第一个 block header** 里 LZMA2 声明的字典大小。
//
// 为什么必须看:xz 库解析 block header 后做的是
//
//	if dc > config.DictCap { config.DictCap = dc }
//
// —— 也就是**文件的声明会抬高**解码器配置,而不是被它限制。实测:把 168 字节
// .xz 里那一个 props 字节改成 40(=4 GiB),Go 就会 make 一个 4 GiB 的字典缓冲;
// 平常靠懒分配只花 16 MiB RSS,但在 ulimit -v / VA 受限的环境里直接
// `runtime: out of memory` —— 那是不可恢复的 fatal error,整个 sync 连带所有项目
// 一起没了,而且信息里不含"是哪个包"。
//
// 覆盖范围要如实说:只检查第一个 block。后续 block 的 header 位置要靠前一个 block
// 的压缩长度,而那个字段在 flags 里是可选的 —— 不吃完整流就定位不到。所以多 block
// 的包(xz -T4 产物)只有第一个被检查;真要恶意构造可以把大声明放进后面的 block。
// 这条不是完整防线,是把"坏包/简单恶意包"从进程死亡变成一句指名的拒绝。
// 结构读不下去时返回 error,调用方**不**据此拒绝(交给解码器判断,避免误杀合法包)。
func xzDictClaim(path string) (int64, error) {
	_, v, err := xzDictProp(path)
	return v, err
}

// xzDictProp 返回第一个 block header 里 LZMA2 props 字节的**文件偏移**与解出来的
// 字典大小。偏移也导出给测试用:测试要改的正是这个字节,靠自己扫字节模式会打到
// 压缩数据里的巧合序列(Go 的 writer 输出就撞上过一次)。
func xzDictProp(path string) (int64, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	head := make([]byte, 13) // 12 字节流头 + block header 的 size 字节
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, 0, err
	}
	if !bytes.HasPrefix(head, []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}) {
		return 0, 0, errors.New("not an xz stream")
	}
	if head[12] == 0 {
		return 0, 0, errors.New("no block header (stream has only an index)")
	}
	// block header 的真实长度 = (size字节+1)*4,最小 4 字节。第一版按"含流头"的长度分配
	// 再索引 full[12],对 Go 自己写出来的包就 panic(slice out of range) —— 这段代码
	// 跑在不可信输入上,所以长度要有下限和上限(xz 规范:1 KiB 以内)。
	blockLen := (int(head[12]) + 1) * 4
	if blockLen < 8 || blockLen > 1024 {
		return 0, 0, fmt.Errorf("implausible xz block header length %d", blockLen)
	}
	block := make([]byte, blockLen)
	block[0] = head[12]
	if _, err := io.ReadFull(f, block[1:]); err != nil {
		return 0, 0, err
	}
	blockAt := int64(12) // block header 在文件里的起点
	crcAt := len(block) - 4
	if crc32.ChecksumIEEE(block[:crcAt]) != binary.LittleEndian.Uint32(block[crcAt:]) {
		return 0, 0, errors.New("block header CRC mismatch") // 解码器也会拒,不必我们猜
	}
	// block header 布局:[size][flags][可选的 compressed/uncompressed size]
	// [filter records...][0x00 终止符][补齐][CRC32]。
	// 那两个可选字段以前被忽略,于是 `xz -T4` 的包(会写尺寸)直接读成"bad filter
	// record" —— 真实文件把守卫骗过去了,而守卫骗过去的后果是误杀。
	pos := 2
	switch (block[1] >> 6) & 3 {
	case 1:
		pos = skipUvarint(block, pos, crcAt)
	case 2:
		pos = skipUvarint(block, pos, crcAt)
	case 3:
		pos = skipUvarint(block, skipUvarint(block, pos, crcAt), crcAt)
	}
	if pos == 0 {
		return 0, 0, errors.New("bad xz block header size fields")
	}
	for pos < crcAt && block[pos] != 0 { // 0x00 = filter 列表结束
		id, n := binary.Uvarint(block[pos:])
		if n <= 0 || pos+n >= crcAt {
			return 0, 0, errors.New("bad filter record")
		}
		pos += n
		sz, n := binary.Uvarint(block[pos:])
		if n <= 0 || pos+n+int(sz) > crcAt {
			return 0, 0, errors.New("bad filter record")
		}
		pos += n
		if id == 0x21 && sz == 1 { // LZMA2:1 字节 props,低位编码字典大小
			return blockAt + int64(pos), lzmaDictCap(block[pos]), nil
		}
		pos += int(sz)
	}
	return 0, 0, errors.New("no LZMA2 filter in the first block header")
}

// skipUvarint 返回跳过 varint 之后的位置;读不下去返回 0 表示失败。
func skipUvarint(b []byte, pos, limit int) int {
	_, n := binary.Uvarint(b[pos:])
	if n <= 0 || pos+n > limit {
		return 0
	}
	return pos + n
}

// lzmaDictCap 按 LZMA2 的编码规则还原字典大小(v >= 40 表示 UINT32_MAX)。
func lzmaDictCap(v byte) int64 {
	if v >= 40 {
		return int64(^uint32(0))
	}
	return int64(2|uint(v&1)) << ((v >> 1) + 11)
}

func untar(path, dst, compression string, strip int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var tr *tar.Reader
	switch compression {
	case "gzip":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		tr = tar.NewReader(gz)
	case "bzip2":
		tr = tar.NewReader(bzip2.NewReader(f))
	case "xz":
		// 先看清楚它声称要多大的字典,再决定要不要解码(见 xzDictClaim)。
		if claim, err := xzDictClaim(path); err == nil && claim > maxXzDictCap {
			return fmt.Errorf("xz archive %s declares a %d MiB LZMA2 dictionary (limit %d MiB): "+
				"refusing to decode a claim this far beyond any real xz preset",
				filepath.Base(path), claim>>20, int64(maxXzDictCap)>>20)
		}
		xr, err := xz.NewReader(f)
		if err != nil {
			return err
		}
		tr = tar.NewReader(xr)
	default:
		tr = tar.NewReader(f)
	}
	skipped := 0
	var blocked []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := trimStrip(filepath.Clean(hdr.Name), strip)
		if name == "" {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		// 名字往外跑、经祖先软链穿出去、或穿过一个刚被我们拒掉的软链,都跳过
		if name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) ||
			escapesRoot(dst, target) || throughSymlink(dst, target) || blockedAncestor(blocked, name) {
			skipped++
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			if !linkTargetInside(dst, name, hdr.Linkname) {
				// 记住它:后面同名路径上的成员是冲着"写穿链接"来的
				blocked = append(blocked, name)
				skipped++
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil && !fileExists(target) {
				return err
			}
		default:
			// 硬链接 / fifo / 字符设备 / PAX 扩展头不落地:os.Link 可以指向解压目录**之外**
			// 的任意已有文件(路径白名单挡不住这种),设备节点在构建机上是提权面。
			// 但不能**静默**丢:少了一半内容还一声不吭,比多一个警告危险得多。
			skipped++
			continue
		}
	}
	if skipped > 0 {
		warn("%d %s of %s skipped: the path escapes the extraction dir, goes through a symlink, or is an unsupported member type",
			skipped, pluralEntry(skipped), filepath.Base(path))
	}
	return nil
}

func unzip(path, dst string, strip int) error {
	skipped := 0
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		name := trimStrip(filepath.Clean(f.Name), strip)
		if name == "" {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(name))
		if name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) ||
			escapesRoot(dst, target) || throughSymlink(dst, target) {
			skipped++
			continue
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		// zip 里的符号链接条目(mode 带 symlink 位)会被写成**普通文件**,内容就是
		// 那段目标文本 —— 这是刻意的:不创建链接就没有链接逃逸。tar 那边不同,它
		// 必须造真符号链接,所以走 linkTargetInside 检查。两条路各有一条测试钉住,
		// 免得后来人"顺手统一一下"把 zip 变成 os.Symlink。
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, f.Mode()&0o777)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(out, src)
		out.Close()
		src.Close()
		if err != nil {
			return err
		}
	}
	if skipped > 0 {
		warn("%d zip %s skipped (path traversal)", skipped, pluralEntry(skipped))
	}
	return nil
}
