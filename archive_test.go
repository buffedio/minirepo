package main

// archive_test.go —— <archive> 的下载/校验/解压。用本地 tar.gz(不联网),
// 但走的是和 http 同一条 download/extract 管线。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ulikunitz/xz"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func tarGz(t *testing.T, dir string, payload map[string]string) string {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	tgz := dir + ".tar.gz"
	f, err := os.Create(tgz)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for name, content := range payload {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gw.Close()
	return tgz
}

func sha256Of(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestArchiveSync(t *testing.T) {
	root := t.TempDir()
	up := filepath.Join(root, "files")
	tgz := tarGz(t, filepath.Join(up, "tool"), map[string]string{
		"tool/bin/hello": "#!/bin/sh\necho hello\n",
		"tool/README":    "docs\n",
	})
	mf := filepath.Join(root, "mf")
	os.MkdirAll(mf, 0o755)
	march := filepath.Join(root, "arch")
	os.MkdirAll(march, 0o755)

	sum := sha256Of(t, tgz)
	// strip=1:tar 里多一层 tool/,解压到 path 时要去掉
	writeFile(t, filepath.Join(mf, "arch.xml"), fmt.Sprintf(`<manifest>
  <archive url="%s" path="prebuilt/tool" sha256="%s" strip="1"/>
</manifest>`, filepath.ToSlash(tgz), sum))
	g(t, mf, "init", "-q", "--initial-branch=master")
	g(t, mf, "add", "-A")
	g(t, mf, "commit", "-q", "-m", "arch")

	chdir(t, march)
	guard(t)
	cmdInit([]string{"-u", mf, "-m", "arch.xml"})
	cmdSync(nil)

	got, err := os.ReadFile(filepath.Join(march, "prebuilt", "tool", "bin", "hello"))
	if err != nil {
		t.Fatalf("strip=1 extraction failed: %v", err)
	}
	if string(got) != "#!/bin/sh\necho hello\n" {
		t.Errorf("extracted content = %q", got)
	}
	// 缓存文件名必须含 URL 摘要:两个同名不同址的包不能共用一个缓存条目
	entries, _ := os.ReadDir(filepath.Join(march, toolDir, downloadsDir))
	if len(entries) != 1 {
		t.Fatalf("want 1 cached file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "tool.tar.gz") {
		t.Errorf("cache name %q must keep the basename for debuggability", name)
	}
	if name == "tool.tar.gz" {
		t.Error("cache name has no url digest: two same-named URLs would evict each other")
	}

	// 第二次 sync 必须命中 stamp,不重新解压、不重新校验
	cmdSync(nil)
	if _, err := os.Stat(filepath.Join(march, "prebuilt", "tool", ".minirepo-archive")); err != nil {
		t.Errorf("no stamp written next to the extracted tree: %v", err)
	}

	// 归档也是 status/list 的一等条目(不是"没人管的目录")
	_, projects, archives := resolved(march, false)
	if len(projects) != 0 || len(archives) != 1 {
		t.Fatalf("resolved = %d projects, %d archives", len(projects), len(archives))
	}
	msg, err := syncArchive(march, archives[0], false, false, true)
	if err != nil || !strings.Contains(msg, "cached") {
		t.Errorf("dry-run on an up-to-date archive should say cached, got %q (%v)", msg, err)
	}
	// --no-fetch 的两种情形:内容已就位(stamp 对得上)就不需要网络;
	// 既没缓存又对不上 stamp,必须拒绝,而不是偷偷联网
	os.RemoveAll(filepath.Join(march, toolDir, downloadsDir))
	if _, err := syncArchive(march, archives[0], false, true, false); err != nil {
		t.Errorf("--no-fetch with a valid stamp must not need the network: %v", err)
	}
	os.WriteFile(filepath.Join(march, "prebuilt", "tool", archiveStamp), []byte("stale"), 0o644)
	if _, err := syncArchive(march, archives[0], false, true, false); err == nil ||
		!strings.Contains(err.Error(), "--no-fetch") {
		t.Errorf("--no-fetch must refuse to download, got err=%v", err)
	}

	// sha256 不匹配必须失败(清单被改动/包被替换的场景)
	bad := archives[0]
	bad.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	os.RemoveAll(filepath.Join(march, "prebuilt", "tool"))
	os.Remove(filepath.Join(march, toolDir, downloadsDir, entries[0].Name()))
	if _, err := syncArchive(march, bad, false, false, false); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("a sha256 mismatch must fail, got err=%v", err)
	}
}

// TestArchivePathIsInsideWorkspace 归档落地路径同样是边界的一部分
func TestArchivePathIsInsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "esc.xml"), `<manifest>
  <archive url="/tmp/x.tar.gz" path="../escape"/>
</manifest>`)
	m := parseManifestFile(dir, "esc.xml")
	mustDie(t, "inside the workspace", func() { resolveArchives(m, "") })
}

// TestArchiveTraversalIsRefused 压缩包同样是不可信输入。名字校验不够:tar 可以先
// 放一个逃逸软链,再用普通成员写穿它。同时**仓内**相对软链(SDK 工具链里的
// lib64 -> lib)必须原样保留,不能一刀切禁止软链。
func TestArchiveTraversalIsRefused(t *testing.T) {
	mk := func(t *testing.T, path string, add func(*tar.Writer)) string {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		tw := tar.NewWriter(f)
		add(tw)
		tw.Close()
		return path
	}
	reg := func(tw *tar.Writer, name, content string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	link := func(tw *tar.Writer, name, target string) {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777})
	}
	dir := t.TempDir()
	tarPath := mk(t, filepath.Join(dir, "pkg.tar"), func(tw *tar.Writer) {
		reg(tw, "pkg/hello", "hi")
		link(tw, "pkg/lib64", "lib") // 合法:指向同一目录树里的 lib
		reg(tw, "pkg/lib/real.txt", "lib\n")
		reg(tw, "pkg/..bashrc", "dotfile\n")  // 名字以 .. 开头的合法文件,不能被误杀
		link(tw, "pkg/evil", "../../../../")  // 逃逸软链
		reg(tw, "pkg/evil/PWNED.txt", "nope") // 想写穿它
		link(tw, "pkg/abs", "/tmp")           // 绝对软链
		reg(tw, "../OUTSIDE.txt", "nope")     // 名字直接往外
	})
	dst := filepath.Join(dir, "out")
	os.MkdirAll(dst, 0o755)
	if err := untar(tarPath, dst, "none", 0); err != nil {
		t.Fatalf("untar: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "pkg", "hello")); err != nil || string(b) != "hi" {
		t.Errorf("legitimate member lost: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "pkg", "lib64"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("in-tree relative symlink must survive (toolchain tars need it): %v", err)
	} else if tgt, _ := os.Readlink(filepath.Join(dst, "pkg", "lib64")); tgt != "lib" {
		t.Errorf("symlink target = %q", tgt)
	}
	if _, err := os.Stat(filepath.Join(dst, "pkg", "..bashrc")); err != nil {
		t.Errorf("dotfile named '..bashrc' was over-rejected: %v", err)
	}
	for _, bad := range []string{
		filepath.Join(dir, "OUTSIDE.txt"),              // 名字逃逸
		filepath.Join(dst, "pkg", "evil", "PWNED.txt"), // 经逃逸软链写入
		filepath.Join(dst, "pkg", "evil"),              // 逃逸软链本身
		filepath.Join(dst, "pkg", "abs", "anything"),   // 绝对软链
	} {
		if _, err := os.Lstat(bad); err == nil {
			t.Errorf("escaped member was extracted: %s", bad)
		}
	}
}

func TestLinkTargetInside(t *testing.T) {
	cases := []struct{ name, link, want string }{
		{"pkg/lib64", "lib", "ok"},
		{"pkg/a/b", "../c", "ok"},
		{"pkg/l", "../..", "no"},
		{"pkg/l", "/tmp", "no"},
		{"pkg/l", "../../etc/passwd", "no"},
	}
	for _, c := range cases {
		root := "/ws"
		got := "ok"
		if !linkTargetInside(root, c.name, c.link) {
			got = "no"
		}
		if got != c.want {
			t.Errorf("linkTargetInside(%q -> %q) = %s, want %s", c.name, c.link, got, c.want)
		}
	}
}

// TestArchiveStampRules 三条与"接管老工作区 + 不重复下载"直接相关的性质:
//  1. 旧版把 stamp 写成 JSON,必须读得懂(否则接管后会误判条目变了、要人 --force)
//  2. 钉了 sha256 时稳态判断不得碰网络、不得重算哈希
//  3. --dry-run 的判定必须和真实 sync 一致:stamp 不匹配时不许报 ok
func TestArchiveStampRules(t *testing.T) {
	root := t.TempDir()
	tgz := tarGz(t, filepath.Join(root, "files", "pkg"), map[string]string{"pkg/a.txt": "A\n"})
	sum := sha256Of(t, tgz)
	dest := filepath.Join(root, "out")
	os.MkdirAll(dest, 0o755)
	a := Archive{URL: filepath.ToSlash(tgz), Path: "out", SHA256: sum}

	// 1) 旧版 JSON stamp 就地放着、缓存目录完全不存在
	writeFile(t, filepath.Join(dest, archiveStamp),
		`{
  "url": "`+filepath.ToSlash(tgz)+`",
  "path": "out",
  "sha256": "`+sum+`",
  "strip": 1,
  "format": ""
}`)
	msg, err := syncArchive(root, a, false, true, false) // noFetch=true:证明没走网络
	if err != nil || !strings.Contains(msg, "cached") {
		t.Fatalf("legacy JSON stamp must be understood, got %q err=%v", msg, err)
	}
	if _, err := syncArchive(root, a, false, true, true); err != nil || !strings.Contains(msg, "cached") {
		t.Fatalf("dry-run must also report cached: %q (%v)", msg, err)
	}

	// 2) stamp 是别的包 -> 稳态判断必须失效,而且 dry-run 要如实说会被拒
	writeFile(t, filepath.Join(dest, archiveStamp),
		`{"url":"x","path":"out","sha256":"`+strings.Repeat("0", 64)+`","strip":1}`)
	if _, err := syncArchive(root, a, false, true, false); err == nil {
		t.Error("a stamp from another archive must not be treated as up to date")
	}
	msg, err = syncArchive(root, a, false, true, true)
	if err != nil || strings.Contains(msg, "ok (cached)") {
		t.Errorf("--dry-run lies about a mismatched stamp: %q", msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("--dry-run should mention what a real sync would demand: %q", msg)
	}

	// 3) 新版自己写的 stamp 必须是同一种 JSON(能被 stampDigest 读回)
	os.RemoveAll(dest)
	if _, err := syncArchive(root, a, false, false, false); err != nil {
		t.Fatalf("fresh extract: %v", err)
	}
	got, ok := stampDigest(filepath.Join(dest, archiveStamp))
	if !ok || got != sum {
		t.Errorf("new stamp unreadable or wrong digest: %q ok=%v", got, ok)
	}
	var doc archiveStampDoc
	raw, _ := os.ReadFile(filepath.Join(dest, archiveStamp))
	if json.Unmarshal(raw, &doc) != nil || doc.URL == "" {
		t.Errorf("stamp must stay human-inspectable JSON: %s", raw)
	}
}

// TestArchiveNoAutoStrip 未写 strip 就不许猜着剥一层:同一个包在 strip 与不 strip
// 之间必须有明确区别,否则布局会随顶层是不是"恰好一个目录"而变。
func TestArchiveNoAutoStrip(t *testing.T) {
	root := t.TempDir()
	tgz := tarGz(t, filepath.Join(root, "files", "tool-1.2"), map[string]string{"tool-1.2/bin/x": "X\n"})
	a := Archive{URL: filepath.ToSlash(tgz), Path: "tool", SHA256: sha256Of(t, tgz)}
	if _, err := syncArchive(root, a, false, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tool", "tool-1.2", "bin", "x")); err != nil {
		t.Errorf("strip was invented: %v", err)
	}
	a2 := a
	a2.Path, a2.Strip = "tool1", 1
	if _, err := syncArchive(root, a2, false, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tool1", "bin", "x")); err != nil {
		t.Errorf("strip=1 must remove exactly one level: %v", err)
	}
}

// xzBomb 造一个"合法框帧 + 正确 CRC,但把 LZMA2 字典声明改成 4 GiB"的 .xz。
// 168 字节的文件换来一次 4 GiB 分配:xz 库是 `if dc > config.DictCap { config.DictCap = dc }`
// —— 文件声明抬高配置,而不是被它限制。
func xzBomb(t *testing.T) []byte {
	t.Helper()
	var packed bytes.Buffer
	xw, err := xz.NewWriter(&packed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := xw.Write(tarOf(t, "pkg/payload.txt", "payload\n")); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
	good := packed.Bytes()
	src := writeTmp(t, good)
	off, claim, err := xzDictProp(src)
	if err != nil || claim > maxXzDictCap {
		t.Fatalf("fixture assumption broke: our own xz output claims %d (%v)", claim, err)
	}
	bad := make([]byte, len(good))
	copy(bad, good)
	bad[off] = 40 // UINT32_MAX 一档
	crcAt := 12 + (int(bad[12]+1))*4 - 4
	binary.LittleEndian.PutUint32(bad[crcAt:], crc32.ChecksumIEEE(bad[12:crcAt]))
	if _, got, err := func() (int64, int64, error) { p := writeTmp(t, bad); return xzDictProp(p) }(); err != nil || got <= maxXzDictCap {
		t.Fatalf("patch did not produce a huge claim: %d (%v)", got, err)
	}
	return bad
}

func writeTmp(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "in.xz")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestXzHugeDictClaimIsRefused 是那条内存放大的收口:变成一句指名的拒绝,而不是
// runtime: out of memory 把整个 sync 带走。
func TestXzHugeDictClaimIsRefused(t *testing.T) {
	bad := xzBomb(t)
	if got, err := xzDictClaim(writeTmp(t, bad)); err != nil || got <= maxXzDictCap {
		t.Fatalf("xzDictClaim missed the claim: %d, %v", got, err)
	}
	dir := t.TempDir()
	arch := filepath.Join(dir, "big-dict.tar.xz")
	if err := os.WriteFile(arch, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	err := untar(arch, filepath.Join(dir, "out"), "xz", 1)
	if err == nil {
		t.Fatal("a 4 GiB dictionary claim was decoded anyway")
	}
	msg := err.Error()
	if !strings.Contains(msg, "dictionary") || !strings.Contains(msg, "limit") {
		t.Errorf("the message must say what was too big and what the bound is: %q", msg)
	}
	if !strings.Contains(msg, "big-dict.tar.xz") {
		t.Errorf("the message must name the file (sync has many archives): %q", msg)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "payload.txt")); err == nil {
		t.Error("the refused archive still extracted")
	}
}

// TestXzDictClaimAcceptsRealFiles 反向半:合法包必须照常通过,包括 `xz -T4` 产生的
// **多 block** 文件(否则这条守卫就是误杀)。
func TestXzDictClaimAcceptsRealFiles(t *testing.T) {
	dir := t.TempDir()
	payload := filepath.Join(dir, "src.tar")
	makeTarFile(t, payload) // 内容够大,--block-size 才切得出多 block

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"default", []string{"--format=xz", "-6"}},
		{"preset9e", []string{"--format=xz", "-9e"}},
		{"multiblock", []string{"--format=xz", "-T4", "--block-size=64KiB", "-6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireXz(t)
			out := payload + "." + tc.name + ".xz"
			args := append(append([]string{}, tc.args...), "-c", payload)
			cmd := exec.Command("xz", args...)
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &bytes.Buffer{}
			if err := cmd.Run(); err != nil {
				t.Fatalf("xz %v failed: %v", tc.args, err)
			}
			if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.name == "multiblock" && xzListBlocks(t, out) < 2 {
				t.Fatalf("this case is supposed to cover multi-block streams, xz produced %d",
					xzListBlocks(t, out))
			}
			claim, err := xzDictClaim(out)
			if err != nil {
				t.Fatalf("cannot read a real xz file: %v", err)
			}
			if claim > maxXzDictCap {
				t.Errorf("a real xz preset %v declares %d MiB: the bound is too tight", tc.name, claim>>20)
			}
			dst := filepath.Join(dir, "out-"+tc.name)
			if err := untar(out, dst, "xz", 1); err != nil {
				t.Fatalf("legit archive refused: %v", err)
			}
		})
	}
}

// xzListBlocks 用 xz CLI 自己数的 block 数当判据(而不是我自己再数一遍字节,
// 那等于用同一个可能错的理解去验证自己)。
func xzListBlocks(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("xz", "--list", "-v", path).CombinedOutput()
	if err != nil {
		t.Fatalf("xz --list: %v\n%s", err, out)
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if fields := strings.Fields(ln); len(fields) == 2 && fields[0] == "Blocks:" {
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				t.Fatalf("cannot parse %q", ln)
			}
			return n
		}
	}
	t.Fatalf("no Blocks: line in xz --list output:\n%s", out)
	return 0
}

func requireXz(t *testing.T) {
	if _, err := exec.LookPath("xz"); err != nil {
		t.Skip("NOT a pass: the xz CLI is missing, so real-preset coverage for this machine was skipped")
	}
}

func makeTarFile(t *testing.T, path string) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// 随机内容才压得动(xz 对全零输入会给出奇怪的 block 切分)
	body := make([]byte, 400000)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	hdr := &tar.Header{Name: "pkg/blob.bin", Mode: 0o644, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
