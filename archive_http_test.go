package main

// archive_http_test.go —— 归档格式矩阵 + 断点续传 + 坏缓存淘汰。
//
// 为什么单独钉这组:`github.com/ulikunitz/xz` 是本仓库唯一的第三方模块(Go 标准库
// 不读 xz)。依赖升级或换实现时,五种格式里任何一种悄悄坏掉,别的测试都不会知道 --
// 不写 <archive> 的用例根本不会走到这条路径。传输用 httptest:端口自动分配、
// 不联网、能精确断言 Range 头。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// refTarBz2: `tar cjf pkg/payload.txt`(内容 content-bz2)的真参考文件,base64。
// 带一层目录是故意的:这样 strip=1/strip=0 两种解法对每种格式都能被区分出来。
// Go 能读 bzip2 但不能写,所以参考文件必须内嵌 -- 用"检测到 bzip2 命令才测"的写法
// 会在某台机器上静默变成不测。
const refTarBz2 = "QlpoOTFBWSZTWTL3qI0AAIL7hMIQAQBAA/+AIAB+jd5wAAIACCAAchpQ00ZAaMhpoDTaglCa" +
	"RMEeSYjQGTE431saCCBREBEJ3Eui8olqoiAzsQ0azPzGkTKhGFUxFxm9rnOdJlIdaoDSWD1O" +
	"4x93J69uPT6fhoKq0j1cY+dpkdpzfSY1SmA0ERAfi7kinChIGXvURoA="

func tarOf(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	return buf.Bytes()
}

// makeArchive 按格式造一个只含 payload.txt 的包。tar.bz2 用内嵌 fixture,所以它的
// 内容写死在 fixture 里,由调用方决定期望值。
func makeArchive(t *testing.T, kind, content string) (data []byte, want string) {
	t.Helper()
	raw := tarOf(t, "pkg/payload.txt", content)
	switch kind {
	case "tar":
		return raw, content
	case "tar.gz":
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		_, _ = gw.Write(raw)
		gw.Close()
		return buf.Bytes(), content
	case "tar.xz":
		var buf bytes.Buffer
		xw, err := xz.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := xw.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := xw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes(), content
	case "tar.bz2":
		b, err := base64.StdEncoding.DecodeString(refTarBz2)
		if err != nil {
			t.Fatalf("embedded bz2 fixture corrupt: %v", err)
		}
		return b, "content-bz2\n"
	case "zip":
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		f, err := zw.Create("pkg/payload.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		zw.Close()
		return buf.Bytes(), content
	}
	t.Fatalf("unknown kind %q", kind)
	return nil, ""
}

// tarGzBytes 打一个含单个大文件的 tar.gz(体积真实,压缩率接近 1)
func tarGzBytes(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	var gb bytes.Buffer
	gw := gzip.NewWriter(&gb)
	if _, err := gw.Write(tb.Bytes()); err != nil {
		t.Fatal(err)
	}
	gw.Close()
	return gb.Bytes()
}

func TestArchiveFormatMatrix(t *testing.T) {
	const content = "hello-format\n"
	for _, kind := range []string{"tar", "tar.gz", "tar.bz2", "tar.xz", "zip"} {
		data, want := makeArchive(t, kind, content)
		// 每个包都带一层 pkg/:strip 有与没有必须能区分出来(zip 曾经只被
		// "包里没有目录"的用例测过,等于没测)
		for _, strip := range []int{1, 0} {
			t.Run(fmt.Sprintf("%s-strip%d", kind, strip), func(t *testing.T) {
				dir := t.TempDir()
				name := map[string]string{
					"tar": "pkg.tar", "tar.gz": "pkg.tar.gz", "tar.bz2": "pkg.tar.bz2",
					"tar.xz": "pkg.tar.xz", "zip": "pkg.zip",
				}[kind]
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
					t.Fatal(err)
				}
				srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
				t.Cleanup(srv.Close)
				sum := sha256.Sum256(data)
				ws := filepath.Join(dir, "ws")
				a := Archive{
					URL: srv.URL + "/" + name, Path: "out",
					SHA256: hex.EncodeToString(sum[:]), Strip: strip,
				}
				msg, err := syncArchive(ws, a, false, false, false)
				if err != nil {
					t.Fatalf("%s: %v (%s)", kind, err, msg)
				}
				rel := filepath.Join("out", "pkg", "payload.txt")
				if strip == 1 {
					rel = filepath.Join("out", "payload.txt")
				}
				got, err := os.ReadFile(filepath.Join(ws, rel))
				if err != nil {
					t.Fatalf("%s strip=%d: expected %s: %v (%s)", kind, strip, rel, err, msg)
				}
				if string(got) != want {
					t.Errorf("%s: payload = %q, want %q", kind, got, want)
				}
				if _, ok := stampDigest(filepath.Join(ws, "out", archiveStamp)); !ok {
					t.Errorf("%s: no stamp -> every later sync would redo the extraction", kind)
				}
				// 稳态:钉了 sha256 就不该再碰网络(--no-fetch 也照样判 ok)
				if msg, err := syncArchive(ws, a, false, true, false); err != nil || !strings.Contains(msg, "cached") {
					t.Errorf("%s: steady state must hit the stamp, got %q (%v)", kind, msg, err)
				}
			})
		}
	}
}

// TestArchiveResume 半截下载必须从断点接上,而不是从头再来:清单挂的往往是几 GB 的
// 工具链,而且第二次请求要真的带 Range 头(只测"最后文件是对的"抓不到重头下载)。
func TestArchiveResume(t *testing.T) {
	dir := t.TempDir()
	payload := make([]byte, 40000)
	if _, err := rand.Read(payload); err != nil { // 随机:可压数据会被 gzip 缩到几十字节,断不到一半
		t.Fatal(err)
	}
	body, _ := makeArchive(t, "tar.gz", "") // 占位,下面用 big.bin 版本覆盖
	_ = body
	body = tarGzBytes(t, "big.bin", payload)
	var seen []string // 每次请求的 Range 头
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		seen = append(seen, rng)
		if rng == "" && len(seen) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			w.WriteHeader(200)
			_, _ = w.Write(body[:15000])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler) // 连接掐断,模拟掉线
		}
		if rng == "" {
			http.ServeContent(w, r, "pkg.tar.gz", time.Time{}, bytes.NewReader(body))
			return
		}
		var start int
		if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil {
			t.Errorf("unparsable Range header %q", rng)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start:])
	}))
	t.Cleanup(srv.Close)

	ws := filepath.Join(dir, "ws")
	a := Archive{URL: srv.URL + "/pkg.tar.gz", Path: "out"}
	if _, err := syncArchive(ws, a, false, false, false); err == nil {
		t.Fatal("an aborted transfer must fail, not report success")
	}
	part := filepath.Join(ws, toolDir, downloadsDir, hashName(a.URL)+".part")
	fi, err := os.Stat(part)
	if err != nil {
		t.Fatalf("nothing kept after a broken transfer: %v", err)
	}
	if fi.Size() == 0 || fi.Size() >= int64(len(body)) {
		t.Errorf("partial size %d not strictly inside (0, %d)", fi.Size(), len(body))
	}
	if fileExists(strings.TrimSuffix(part, ".part")) {
		t.Fatal("a truncated download was promoted to the cache -- it would then fail sha256 forever")
	}

	n := len(seen)
	if _, err := syncArchive(ws, a, false, false, false); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if len(seen) <= n {
		t.Fatal("second attempt issued no request")
	}
	if !strings.HasPrefix(seen[n], "bytes=") {
		t.Errorf("resumed request restarted from 0 (Range=%q): a 3GB toolchain would re-download", seen[n])
	}
	got, err := os.ReadFile(filepath.Join(ws, "out", "big.bin"))
	if err != nil {
		t.Fatalf("payload after resume: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("assembled payload wrong: %d bytes vs %d", len(got), len(payload))
	}
	if fileExists(part) {
		t.Error(".part left behind after a completed download")
	}
}

// TestArchiveStaleCacheEvicted 厂商覆盖上传同一个 URL(SDK 发布常见):缓存内容
// 对不上清单钉的 sha256 时必须删掉坏缓存,否则每次 sync 都在同一份坏文件上失败。
func TestArchiveStaleCacheEvicted(t *testing.T) {
	dir := t.TempDir()
	data, _ := makeArchive(t, "tar.gz", "old\n")
	a := Archive{URL: filepath.ToSlash(filepath.Join(dir, "pkg.tar.gz")), Path: "out", SHA256: strings.Repeat("f", 64)}
	if err := os.WriteFile(a.URL, data, 0o644); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(dir, "ws")
	if _, err := syncArchive(ws, a, false, false, false); err == nil ||
		!strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha256 mismatch, got %v", err)
	}
	cache := filepath.Join(ws, toolDir, downloadsDir, hashName(a.URL))
	if fileExists(cache) {
		t.Error("the mismatching cache entry survived; every later sync re-fails on it")
	}
}

// TestDownloadStallIsDetected 镜像站接了连接、发了个头就不再给数据:没有看门狗的话
// `sync` 会永久挂住(py 版为此专门有下载超时,移植时一度丢了)。
// 只防"卡住"不防"慢":3GB 的包本来要跑几分钟,所以断言的是**空闲**超时。
func TestDownloadStallIsDetected(t *testing.T) {
	origIdle, origWarn := readIdleTimeout, warn
	readIdleTimeout = 300 * time.Millisecond
	var warned []string
	warn = func(f string, a ...any) { warned = append(warned, fmt.Sprintf(f, a...)) }
	t.Cleanup(func() { readIdleTimeout, warn = origIdle, origWarn })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("first-chunk"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 头和数据都到了:任何"响应头超时"都救不了这种卡法
		}
		<-release // 然后永远不给下一个字节
	}))
	// 顺序很重要:httptest 的 Close 会等待在途请求结束,所以必须先放行 handler。
	// (写反过一次的后果是测试永久挂住 -- 而"能挂住的测试"本身就是缺陷。)
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	dst := filepath.Join(t.TempDir(), "pkg.tar.gz")
	type result struct {
		err error
		el  time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		err := download(srv.URL+"/pkg.tar.gz", dst)
		done <- result{err, time.Since(start)}
	}()
	var res result
	select {
	case res = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("download() never returned: the stall guard does not work (this is the permanent hang)")
	}
	err, el := res.err, res.el
	if err == nil {
		t.Fatal("a stalled transfer reported success")
	}
	if fileExists(dst) {
		t.Error("a stalled download was promoted to the cache")
	}
	if el > 10*time.Second {
		t.Errorf("took %v: the watchdog did not fire (this is a permanent hang)", el)
	}
	if el < 100*time.Millisecond {
		t.Errorf("failed in %v -- too fast to be the idle guard; something else broke: %v", el, err)
	}
	joined := strings.Join(warned, "\n")
	if !strings.Contains(joined, "stalled") {
		t.Errorf("user gets no explanation of why it gave up: %q", joined)
	}
	// 半截要留着:下一次 sync 从这儿续,而不是从 0 开始
	if _, err := os.Stat(dst + ".part"); err != nil {
		t.Errorf("partial file dropped: %v", err)
	}
}

// TestDownloadKeepsProxySupport 自定义 Transport 会**顶掉** DefaultTransport 的
// http.ProxyFromEnvironment:忘了写这行,http_proxy 就地失效,而症状是"在内网机器
// 上连不出去"这种最难查的东西。
func TestDownloadKeepsProxySupport(t *testing.T) {
	tr, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, expected *http.Transport", httpClient.Transport)
	}
	if tr.Proxy == nil {
		t.Error("Proxy is nil: http_proxy/no_proxy silently stop working")
	}
	if tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 || tr.DialContext == nil {
		t.Error("one of the hang guards is unset")
	}
}

// TestDownloadSlowButSteadyCompletes 空闲超时不能变成"总时长超时":一个每分钟只挪
// 几 KB 的内网镜像必须**能下完**。这条是上一行的镜像面 -- 只看"卡住就报错"的测试
// 抓不到"读到数据却忘了刷新时间戳"那种误杀正常下载的写法。
func TestDownloadSlowButSteadyCompletes(t *testing.T) {
	orig := readIdleTimeout
	readIdleTimeout = 250 * time.Millisecond
	t.Cleanup(func() { readIdleTimeout = orig })

	const chunks, gap = 10, 60 * time.Millisecond // 总时长 ~600ms = 空闲阈值的 2.4 倍
	body := make([]byte, chunks*1024)
	for i := range body {
		body[i] = byte('a' + i%26)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(200)
		for i := 0; i < chunks; i++ {
			if _, err := w.Write(body[i*1024 : (i+1)*1024]); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(gap) // 每次间隔都小于空闲阈值
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	dst := filepath.Join(dir, "pkg.tar.gz")
	start := time.Now()
	if err := download(srv.URL+"/pkg.tar.gz", dst); err != nil {
		t.Fatalf("a slow but live transfer was killed after %v: %v", time.Since(start), err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("trickled file corrupt: %d bytes vs %d", len(got), len(body))
	}
	if time.Since(start) < chunks*gap/2 {
		t.Errorf("returned in %v: the server-side sleeps did not actually run, so this proves nothing", time.Since(start))
	}
}

// TestInterruptedExtractLeavesNoGarbage 打的是"被 kill 之后"的另一半:解压写
// <dest>.minirepo-tmp、成功才原子替换,所以中途 kill -9 必然留下这个目录。它用的是
// **固定名**并在每次解压开头 RemoveAll,因此垃圾上限就是一个目录、且下一次一定收走。
// 这条以前没测:不测的话"改成随机临时名"这种写法会看起来无害,实际让每次中断都永久
// 多留一个目录。
func TestInterruptedExtractLeavesNoGarbage(t *testing.T) {
	dir := t.TempDir()
	archPath := filepath.Join(dir, "pkg.tar.gz")
	data, payload := makeArchive(t, "tar.gz", "v1\n")
	if err := os.WriteFile(archPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	a := Archive{URL: filepath.ToSlash(archPath), Path: "out", SHA256: sha256Of(t, archPath), Strip: 1}
	ws := filepath.Join(dir, "ws")
	stale := filepath.Join(ws, "out.minirepo-tmp")
	// 上一次被打断的样子:半个解压结果
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "half-written.txt"), []byte("junk"), 0o644)

	if msg, err := syncArchive(ws, a, false, false, false); err != nil {
		t.Fatalf("sync after an interrupted extract failed: %v (%s)", err, msg)
	}
	if fileExists(stale) {
		t.Error("the stale .minirepo-tmp from the interrupted run is still there (it will never be cleaned)")
	}
	got, err := os.ReadFile(filepath.Join(ws, "out", "payload.txt"))
	if err != nil {
		t.Fatalf("extraction did not land: %v", err)
	}
	if string(got) != payload {
		t.Errorf("content wrong: %q", got)
	}

	// 换内容再同步一次(这次是"重解压中途被打断"):旧内容必须活到新解压成功为止
	// 第二个包换个 URL:同一个 URL 换内容会被缓存语义**正确地**拒掉(见
	// TestArchiveStaleCacheEvicted),这里要测的是重解压的临时目录,不是那条。
	archPath2 := filepath.Join(dir, "pkg2.tar.gz")
	data2, payload2 := makeArchive(t, "tar.gz", "v2\n")
	if err := os.WriteFile(archPath2, data2, 0o644); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(stale, 0o755)
	os.WriteFile(filepath.Join(stale, "half2.txt"), []byte("junk"), 0o644)
	a.URL = filepath.ToSlash(archPath2)
	a.SHA256 = sha256Of(t, archPath2)
	if msg, err := syncArchive(ws, a, true, false, false); err != nil {
		t.Fatalf("re-extract failed: %v (%s)", err, msg)
	}
	if fileExists(stale) {
		t.Error("stale .minirepo-tmp left behind on the re-extract path too")
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "out", "payload.txt")); string(got) != payload2 {
		t.Errorf("re-extract did not replace content: %q", got)
	}

	// 解压**失败**时也不能留垃圾:否则一次坏的包永久占着一份解压目录
	bad := filepath.Join(dir, "bad.tar.gz")
	os.WriteFile(bad, []byte("not an archive at all"), 0o644)
	ab := Archive{URL: filepath.ToSlash(bad), Path: "bad", Strip: 1}
	if _, err := syncArchive(ws, ab, false, false, false); err == nil {
		t.Fatal("a non-archive reported success")
	}
	if fileExists(filepath.Join(ws, "bad.minirepo-tmp")) {
		t.Error("a failed extraction left its temp dir behind")
	}
}
