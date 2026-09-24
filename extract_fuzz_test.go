package main

// 解压不可信归档的 fuzz 与防线测试。归档内容是完全的外部输入,而这里的失败模式是
// "往工作区外面写文件"或"留下一个指向工作区外的符号链接"。
//
// 两个 fuzz target:
//   - FuzzExtract:结构**合法**的包(tar / tar.gz / tar.xz / zip)+ 可变的 strip,
//     所以变异能真的走到 trimStrip、越界跳过、blocked 写穿那几条分支上。以前种子
//     只有 zip 且大多死在 OpenReader,938k 次执行的阴性结果其实很弱。
//   - FuzzXzHeader:守卫自己也是解析不可信输入的代码(它第一版就 panic 过),
//     它比被它守的东西更需要 fuzz。
//
// 判据全部**独立**于生产代码:用 Rel/Lstat 自己判"出界"和"写穿链接",而不是复用
// escapesRoot/blocked —— 否则只是在问"函数和它自己一致吗"。
//
//   go test -run FuzzExtract              # 种子
//   go test -fuzz FuzzExtract -fuzztime 60s

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// ---------- 构造归档(返回 error,这样 *testing.F 也能用) ----------

type tarEntry struct {
	name string
	typ  byte
	body string
}

type zipEntry struct {
	name    string
	body    string
	symlink bool
}

func tarBytes(entries ...tarEntry) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: e.typ}
		if e.typ == tar.TypeSymlink || e.typ == tar.TypeLink {
			hdr.Linkname = e.body
			hdr.Size = 0
			hdr.Mode = 0o777
		}
		if e.typ == tar.TypeDir {
			hdr.Size = 0
			hdr.Mode = 0o755 // 生产代码固定用 0755 建目录,这里要跟着,否则写不进子文件
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// buildTar 是手写用例侧的薄包装(要 tarBytes 还是给 error,两边不各写一份格式代码)。
func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	b, err := tarBytes(entries...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func zipBytes(entries ...zipEntry) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.symlink {
			hdr.SetMode(0o777 | 0o120000) // 符号链接:目标放在文件内容里
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildZip(entries ...zipEntry) []byte {
	b, err := zipBytes(entries...)
	if err != nil {
		panic(err) // 只在构造种子时用;失败就是测试自己的错
	}
	return b
}

func gzBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(raw); err != nil {
		return nil, err
	}
	// 必须先 Close 再取 Bytes():Go 的 return 语句按顺序求值,写
	// `return buf.Bytes(), gw.Close()` 会交出**没有尾巴**的流(实测解压报
	// unexpected EOF)—— 而这正是 fuzz 种子最不该有的毛病:种子是坏的,变异就全
	// 花在重新拼结构上,覆盖面看着大其实空。
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func xzBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if _, err := xw.Write(raw); err != nil {
		return nil, err
	}
	if err := xw.Close(); err != nil { // 同上:先 Close 再取字节
		return nil, err
	}
	return buf.Bytes(), nil
}

func zipMirror(entries []tarEntry) []zipEntry {
	out := make([]zipEntry, 0, len(entries))
	for _, e := range entries {
		if e.typ == tar.TypeDir {
			continue // zip 靠名字里的 '/' 隐含目录
		}
		out = append(out, zipEntry{name: e.name, body: e.body, symlink: e.typ == tar.TypeSymlink})
	}
	return out
}

// hostileMembers 覆盖解压的每一条分支:越界(`../`)、绝对路径、strip 削完变空或变
// 越界、真符号链接逃逸、以及"先放一个指向上的链接、再往它下面写"的写穿序列。
func hostileMembers() []tarEntry {
	return []tarEntry{
		{name: "pkg/", typ: tar.TypeDir},
		{name: "pkg/ok.txt", typ: tar.TypeReg, body: "good"},
		{name: "pkg/../escape.txt", typ: tar.TypeReg, body: "bad"},
		{name: "../../outside.txt", typ: tar.TypeReg, body: "bad"},
		{name: "/abs/member.txt", typ: tar.TypeReg, body: "bad"},
		{name: "pkg/deep/./x/../y.txt", typ: tar.TypeReg, body: "ok"},
		{name: "pkg/inlink", typ: tar.TypeSymlink, body: "ok.txt"},
		{name: "pkg/uplink", typ: tar.TypeSymlink, body: "../../../etc/passwd"},
		{name: "pkg/abslink", typ: tar.TypeSymlink, body: "/etc/shadow"},
		{name: "poison", typ: tar.TypeSymlink, body: "../.."},
		{name: "poison/pwn.txt", typ: tar.TypeReg, body: "write-through"},
		{name: "only/a/b/c.txt", typ: tar.TypeReg, body: "deep"},
	}
}

// ---------- 独立判据 ----------

func assertInsideRoot(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			t.Fatalf("cannot relate %q to root %q: %v", path, root, rerr)
		}
		if rel == "." {
			return nil
		}
		if rel == ".." || startsUp(rel) {
			t.Fatalf("extraction wrote outside the destination: %q (rel %q)", path, rel)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, lerr := os.Readlink(path)
			if lerr != nil {
				return lerr
			}
			abs := target
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(filepath.Dir(path), target)
			}
			if filepath.IsAbs(target) && !inside(target, root) {
				t.Fatalf("absolute symlink escapes: %q -> %q", path, target)
			}
			if rel2, rerr2 := filepath.Rel(root, abs); rerr2 == nil && (rel2 == ".." || startsUp(rel2)) {
				t.Fatalf("symlink %q points outside the destination: -> %q", path, target)
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Logf("walk: %v", err) // 解压失败留下的半截目录不算新信息
	}
}

// assertNothingWrittenThroughSymlink:普通文件的某个祖先若是符号链接,说明解压跟着
// 链接把数据写到了别处(生产代码用 blocked 名单防这个,这里独立复查)。
func assertNothingWrittenThroughSymlink(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
		for a := filepath.Dir(path); len(a) > len(root) && a != root; a = filepath.Dir(a) {
			fi, serr := os.Lstat(a)
			if serr != nil {
				break
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				t.Errorf("%q was written **through** the symlink %q", path, a)
				break
			}
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Logf("walk: %v", err)
	}
}

// skippedCount 从警告里取出被跳过的成员数(没有警告=0=静默丢弃,那才是要抓的)。
// 它同时要求这句话是**正常可读的**:"1 zip entr(y/ies) skipped" 这种在格式串里手写
// 单复数的写法,Go 会原样打印出来 —— 而 ASCII 检查抓不到它,只有把消息当句子解析才抓得到。
func skippedCount(t *testing.T, warned string) int {
	for _, ln := range strings.Split(warned, "\n") {
		head, rest, found := strings.Cut(ln, " ")
		if !found {
			continue
		}
		n, err := strconv.Atoi(head)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(rest, "entry ") && !strings.HasPrefix(rest, "entries ") &&
			!strings.HasPrefix(rest, "zip entry ") && !strings.HasPrefix(rest, "zip entries ") {
			t.Errorf("warning is not a readable sentence: %q", ln)
			return 0
		}
		return n
	}
	return 0
}

func startsUp(rel string) bool { return rel == ".." || strings.HasPrefix(rel, "../") }
func inside(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && !startsUp(rel) && rel != ".."
}

func checkExtract(t *testing.T, data []byte, strip int) {
	t.Helper()
	dir := t.TempDir()
	arch := filepath.Join(dir, "pkg")
	if err := os.WriteFile(arch, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	// 走**真实入口**(含格式嗅探)。越界成员被跳过、或整包被拒,都是允许的结果;
	// 崩溃和把文件写到界外不是。
	parseSafely(t, func() {
		_ = extractArchive(arch, dst, strip)
	})
	assertInsideRoot(t, dst)
	assertNothingWrittenThroughSymlink(t, dst)
}

// ---------- fuzz ----------

func FuzzExtract(f *testing.F) {
	raw, err := tarBytes(hostileMembers()...)
	if err != nil {
		f.Fatal(err)
	}
	tgz, err := gzBytes(raw)
	if err != nil {
		f.Fatal(err)
	}
	txz, err := xzBytes(raw)
	if err != nil {
		f.Fatal(err)
	}
	zips, err := zipBytes(zipMirror(hostileMembers())...)
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range [][]byte{raw, tgz, txz, zips} {
		for _, strip := range []uint8{0, 1, 2, 9} {
			f.Add(seed, strip)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, strip uint8) {
		if len(data) > 64*1024 {
			t.Skip("pathological input size")
		}
		checkExtract(t, data, int(strip))
	})
}

// ---------- 手写用例:实际语义是"恶意成员跳过 + 警告",不是整包报错 ----------

func TestExtractSkipsHostileMembers(t *testing.T) {
	origWarn := warn
	var warned []string
	warn = func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }
	t.Cleanup(func() { warn = origWarn })

	for _, tc := range []struct {
		name        string
		kind        string // "zip" | "tar":决定用哪个解压器
		write       func(t *testing.T, path string)
		mustNot     []string
		mustHave    []string
		mustContent map[string]string // 内容判据:"没被覆盖"只能靠内容看出来
		wantSkip    int               // 至少几条被数进警告;确切数字是实现细节,0 才是 bug
	}{
		{
			name: "zip zip-slip", kind: "zip",
			write: func(t *testing.T, p string) {
				os.WriteFile(p, buildZip(
					zipEntry{name: "pkg/ok.txt", body: "good"},
					zipEntry{name: "pkg/../../outside.txt", body: "bad"}), 0o600)
			},
			mustNot:  []string{"outside.txt", "../outside.txt"},
			mustHave: []string{"pkg/ok.txt"},
			wantSkip: 1,
		},
		{
			name: "zip absolute member", kind: "zip",
			write: func(t *testing.T, p string) {
				os.WriteFile(p, buildZip(zipEntry{name: "/tmp/zz-never.txt", body: "bad"}), 0o600)
			},
			mustNot:  []string{},
			wantSkip: 1,
		},
		{
			name: "tar symlink escape", kind: "tar",
			write: func(t *testing.T, p string) {
				os.WriteFile(p, buildTar(t,
					tarEntry{name: "pkg/", typ: tar.TypeDir},
					tarEntry{name: "pkg/l", typ: tar.TypeSymlink, body: "../../../etc/passwd"},
					tarEntry{name: "pkg/ok.txt", typ: tar.TypeReg, body: "good"}), 0o600)
			},
			mustNot:  []string{"pkg/l"},
			mustHave: []string{"pkg/ok.txt"},
			wantSkip: 1,
		},
		{
			// 链接指向**界内**(所以允许创建),然后有成员写穿它去覆盖真文件。
			// throughSymlink 防的就是这个;而"逃逸"类断言看不见它 —— 覆盖发生在界内。
			name: "tar clobber through an inside symlink", kind: "tar",
			write: func(t *testing.T, p string) {
				os.WriteFile(p, buildTar(t,
					tarEntry{name: "pkg/", typ: tar.TypeDir},
					tarEntry{name: "pkg/real.txt", typ: tar.TypeReg, body: "good"},
					tarEntry{name: "link", typ: tar.TypeSymlink, body: "pkg"},
					tarEntry{name: "link/real.txt", typ: tar.TypeReg, body: "EVIL"}), 0o600)
			},
			mustHave:    []string{"pkg/real.txt", "link"},
			mustContent: map[string]string{"pkg/real.txt": "good"},
			wantSkip:    1,
		},
		{
			name: "tar write-through-symlink poison", kind: "tar",
			write: func(t *testing.T, p string) {
				os.WriteFile(p, buildTar(t,
					tarEntry{name: "pkg/ok.txt", typ: tar.TypeReg, body: "good"},
					tarEntry{name: "l", typ: tar.TypeSymlink, body: "../.."},
					tarEntry{name: "l/pwn.txt", typ: tar.TypeReg, body: "nope"}), 0o600)
			},
			mustNot:  []string{"l/pwn.txt"},
			mustHave: []string{"pkg/ok.txt"},
			wantSkip: 1,
		},
		{
			name: "tar hardlink and fifo are not silently dropped", kind: "tar",
			write: func(t *testing.T, p string) {
				os.WriteFile(p, buildTar(t,
					tarEntry{name: "pkg/ok.txt", typ: tar.TypeReg, body: "good"},
					tarEntry{name: "pkg/h", typ: tar.TypeLink, body: "/etc/shadow"},
					tarEntry{name: "pkg/f", typ: tar.TypeFifo}), 0o600)
			},
			mustNot:  []string{"pkg/h", "pkg/f"},
			mustHave: []string{"pkg/ok.txt"},
			wantSkip: 2, // 一条硬链接 + 一条 fifo
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warned = nil
			dir := t.TempDir()
			arch := filepath.Join(dir, "p")
			tc.write(t, arch)
			dst := filepath.Join(dir, "out")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			// strip=0:成员名与落地路径一一对应,断言才有意义(strip=1 时
			// "pkg/../../x" 会被削成 "x",那是**落在界内**,不是逃逸)。
			msg := parseSafely(t, func() {
				var err error
				if tc.kind == "zip" {
					err = unzip(arch, dst, 0)
				} else {
					err = untar(arch, dst, "", 0)
				}
				if err != nil {
					t.Errorf("extraction failed outright (%v); hostile members should be skipped, the rest kept", err)
				}
			})
			if msg != "" {
				t.Fatalf("extraction died instead of skipping: %q", msg)
			}
			for _, rel := range tc.mustNot {
				if fileExists(filepath.Join(dst, filepath.FromSlash(rel))) {
					t.Errorf("%q was extracted into the workspace", rel)
				}
			}
			for _, rel := range tc.mustHave {
				if !fileExists(filepath.Join(dst, filepath.FromSlash(rel))) {
					t.Errorf("good member %q went missing", rel)
				}
			}
			for rel, want := range tc.mustContent {
				b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
				if err != nil || string(b) != want {
					t.Errorf("%q must keep its content (clobbered through a link?): %q err=%v", rel, b, err)
				}
			}
			joined := strings.Join(warned, "\n")
			if got := skippedCount(t, joined); got < tc.wantSkip {
				t.Errorf("hostile members must be counted in a warning (want >= %d skipped), got %q",
					tc.wantSkip, joined)
			}
			assertInsideRoot(t, dst)
			assertNothingWrittenThroughSymlink(t, dst)
		})
	}
	if fileExists("/tmp/zz-never.txt") {
		t.Error("an absolute zip member was written to /tmp")
	}
}

// TestZipSymlinkEntryLandsAsPlainFile 钉住一个**刻意的**不一致:zip 的符号链接条目
// 写成普通文件(不建链接 => 无逃逸面),tar 才建真链接。有人"统一两边行为"时这条会红,
// 逼他重新看一遍安全问题。
func TestZipSymlinkEntryLandsAsPlainFile(t *testing.T) {
	dir := t.TempDir()
	arch := filepath.Join(dir, "p.zip")
	os.WriteFile(arch, buildZip(zipEntry{name: "pkg/l", body: "../../../etc/passwd", symlink: true}), 0o600)
	dst := filepath.Join(dir, "out")
	os.MkdirAll(dst, 0o755)
	if err := unzip(arch, dst, 1); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "pkg", "l"))
	if err != nil {
		if os.IsNotExist(err) {
			return // 被跳过也可以接受(两种安全结局之一)
		}
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("zip symlink entry was created as a real symlink: the plain-file choice was deliberate")
	}
}

// TestSniffDrivesTheDecoder 钉住 extractArchive 这条路:格式判定错了就会解出垃圾或
// 根本不报错,所以判定本身要有断言(URL 后缀一律不看)。
func TestSniffDrivesTheDecoder(t *testing.T) {
	raw, err := tarBytes(tarEntry{name: "pkg/ok.txt", typ: tar.TypeReg, body: "good"})
	if err != nil {
		t.Fatal(err)
	}
	tgz, err := gzBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	txz, err := xzBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	zips, err := zipBytes(zipEntry{name: "pkg/ok.txt", body: "good"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, kind string
		data       []byte
	}{
		{"plain", "tar", raw}, {"gzip", "gzip", tgz}, {"xz", "xz", txz}, {"zip", "zip", zips},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// 后缀故意写错:判定只能来自内容。
			p := filepath.Join(dir, "misnamed.bin")
			if err := os.WriteFile(p, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := sniffArchive(p)
			if err != nil {
				t.Fatalf("sniff failed: %v", err)
			}
			if got == "" || got == "unknown" {
				t.Fatalf("sniff gave %q for a real %s stream", got, tc.name)
			}
			dst := filepath.Join(dir, "out")
			os.MkdirAll(dst, 0o755)
			if err := extractArchive(p, dst, 1); err != nil {
				t.Fatalf("extractArchive refused a legit %s archive: %v", tc.name, err)
			}
			b, rerr := os.ReadFile(filepath.Join(dst, "ok.txt"))
			if rerr != nil || string(b) != "good" {
				t.Errorf("%s: wrong content %q (%v)", tc.name, b, rerr)
			}
		})
	}
	// 完全不认识的字节必须报错,而不是"解出个空目录"
	dir := t.TempDir()
	p := filepath.Join(dir, "junk.bin")
	os.WriteFile(p, []byte("just some text, not an archive"), 0o600)
	if err := extractArchive(p, filepath.Join(dir, "o2"), 1); err == nil {
		t.Error("a non-archive was accepted")
	} else if !strings.Contains(err.Error(), "archive") {
		t.Errorf("message must say it is about archives: %q", err)
	}
}

// FuzzXzHeader 打的是 xz 字典声明守卫本身:它跑在不可信归档之前,而它的第一版就
// panic 过(`index out of range [12]`)。守卫自己崩比它要防的问题更糟 —— 整个 sync
// 一起死,而且死因是 minirepo 的代码。
func FuzzXzHeader(f *testing.F) {
	raw, err := tarBytes(tarEntry{name: "pkg/a.txt", typ: tar.TypeReg, body: "x"})
	if err != nil {
		f.Fatal(err)
	}
	packed, err := xzBytes(raw)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(packed)
	f.Add([]byte{0xFD, '7', 'z', 'X', 'Z', 0x00, 0, 0, 0, 0, 0, 0})
	f.Add(append([]byte{0xFD, '7', 'z', 'X', 'Z', 0x00}, make([]byte, 3000)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64*1024 {
			t.Skip("pathological input size")
		}
		path := filepath.Join(t.TempDir(), "in.xz")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		off, claim, err := xzDictProp(path)
		if err != nil {
			return // 拒绝是允许的结果:它不许崩
		}
		if claim < 0 || claim > int64(^uint32(0)) {
			t.Fatalf("nonsense dict claim: %d", claim)
		}
		if off < 12 || off >= int64(len(data)) {
			t.Fatalf("props offset %d outside the %d-byte file", off, len(data))
		}
	})
}
