package main

// manifest.go —— repo XML 的「已声明子集」解析器(子集边界见 CONTRACT.md)
//
// 支持的元素/属性(完整清单, CONTRACT.md 是验收标准):
//   <remote name fetch />
//   <default remote revision sync-c />
//   <project name path remote revision url sync-c>  内可含 <copyfile src dest/>
//   <include name />
//   <remove-project name />
//   <archive url path sha256 strip format />        (minirepo 扩展,替代 git-lfs 场景)
//
// 不在表内的元素/属性:一次性 warn 后忽略 —— 子集边界必须可见,不许静默吞掉
// (linkfile/groups 被"支持"过头的工具静默丢弃,排查起来极其痛苦)。
//
// <include> 合并语义(与 repo 一致):被包含文件是「底层默认值」,包含者优先 ——
// remote/default 里同名键父文件赢,projects/archives 追加。
// <remove-project> 按文档顺序生效:只裁掉在它之前定义(含 include 带入)的项目,
// 之后同名重新定义是合法的(repo 的 remove-then-readd 模式)。

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Default struct {
	Remote    string
	Revision  string
	SyncC     bool
	syncCSeen bool // 显式写过 sync-c(含 false);没写才能被子清单继承
}

// RawProject 解析期形态;SyncC 三态(nil=未写,继承 default)
type RawProject struct {
	Name      string
	Path      string
	Remote    string
	Revision  string
	URL       string
	SyncC     *bool
	Copyfiles []Copyfile
	Dead      bool // 被 <remove-project> 裁掉
}

type Copyfile struct{ Src, Dest string }

type RawArchive struct {
	URL, Path, SHA256, Format string
	Strip                     int
}

type Manifest struct {
	Remotes  map[string]string // name -> fetch(可能还是相对 URL,resolve 阶段锚定)
	Default  Default
	Projects []RawProject
	Archives []RawArchive
	Ignored  map[string]bool // "<linkfile>" / "project@groups" 等,顶层一次性警告
}

var supportedAttrs = map[string][]string{
	"remote":         {"name", "fetch"},
	"default":        {"remote", "revision", "sync-c"},
	"project":        {"name", "path", "remote", "revision", "url", "sync-c"},
	"include":        {"name"},
	"remove-project": {"name"},
	"copyfile":       {"src", "dest"},
	"archive":        {"name", "url", "path", "sha256", "strip", "format"},
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

// checkAttrs 把不支持属性记入 Ignored
func checkAttrs(m *Manifest, tag string, t xml.StartElement) {
	for _, a := range t.Attr {
		ok := false
		for _, s := range supportedAttrs[tag] {
			if a.Name.Local == s {
				ok = true
				break
			}
		}
		if !ok {
			m.Ignored[tag+"@"+a.Name.Local] = true
		}
	}
}

func parseBool(s string) *bool {
	b := strings.EqualFold(s, "true") || s == "1"
	return &b
}

// maxManifestFiles 一次解析最多打开多少个清单文件。
// <include> 的指数展开是真实风险:每层把同一个文件引用两次,20 层就是 100 万次
// 解析(实测 18 层已 4s、22 层约 108s、30 层要跑几天),而它看起来只是"几十个
// 小文件"。清单来自第三方,所以这必须是个上限而不是等它卡死。
const maxManifestFiles = 256

func parseManifestFile(manifestsRoot, relpath string) *Manifest {
	parsed := 0
	return parseManifestRec(manifestsRoot, relpath, nil, &parsed)
}

func parseManifestRec(manifestsRoot, relpath string, visited []string, parsed *int) *Manifest {
	if *parsed >= maxManifestFiles {
		die("manifest expands into more than %d files (check for an <include> that is "+
			"referenced at more than one level: that grows exponentially): %s",
			maxManifestFiles, relpath)
	}
	*parsed++
	full := filepath.Join(manifestsRoot, filepath.FromSlash(relpath))
	if !insidePath(manifestsRoot, full) {
		// 子目录清单引用同仓文件允许 "../",但绝不能出仓(realpath 防符号链接)
		die("manifest path escapes the manifests repo: %s", relpath)
	}
	if !fileExists(full) {
		die("manifest file not found: %s (inside %s)", relpath, manifestsRoot)
	}
	for _, v := range visited {
		if v == full {
			die("circular <include> detected at %s", relpath)
		}
	}
	visited = append(visited, full)

	m := &Manifest{Remotes: map[string]string{}, Ignored: map[string]bool{}}
	f, err := os.Open(full)
	if err != nil {
		die("cannot read manifest %s: %v", relpath, err)
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	// Go 的 XML decoder 对"整篇没有任何元素"的文档不报错,未知根元素也只会被当成
	// 未支持特性跳过 -- 于是 "this is not a manifest" 会静默变成一个 0 项目的
	// 工作区。根元素必须是 <manifest>,否则明确失败。
	var sawRoot bool
	var cur *RawProject // 正在收集属性的 <project>
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			die("malformed manifest %s: %v", relpath, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			tag := t.Name.Local
			if !sawRoot {
				sawRoot = true
				if tag != "manifest" {
					die("%s is not a repo manifest: root element is <%s>", relpath, tag)
				}
			}
			if cur != nil { // <project> 的子元素只认 <copyfile>
				if tag == "copyfile" {
					checkAttrs(m, "copyfile", t)
					src, dst := attr(t, "src"), attr(t, "dest")
					if src != "" && dst != "" {
						cur.Copyfiles = append(cur.Copyfiles, Copyfile{Src: src, Dest: dst})
					} else {
						die("<copyfile> needs 'src' and 'dest': %s", relpath)
					}
				} else {
					m.Ignored["<"+tag+">"] = true
					dec.Skip() // 未知子元素的整棵子树都跳过,别把它的孩子也误当 copyfile
				}
				continue
			}
			switch tag {
			case "manifest":
				// 根元素:不警告、不跳过,子元素照常处理
			case "remote":
				checkAttrs(m, tag, t)
				name, fetch := attr(t, "name"), attr(t, "fetch")
				if name == "" || fetch == "" {
					die("<remote> needs 'name' and 'fetch': %s", relpath)
				}
				m.Remotes[name] = fetch // 包含者后定义则覆盖(文档顺序,父文件优先)
			case "default":
				checkAttrs(m, tag, t)
				if v := attr(t, "remote"); v != "" {
					m.Default.Remote = v
				}
				if v := attr(t, "revision"); v != "" {
					m.Default.Revision = v
				}
				if v := attr(t, "sync-c"); v != "" {
					m.Default.SyncC = *parseBool(v)
					m.Default.syncCSeen = true
				}
			case "project":
				checkAttrs(m, tag, t)
				p := &RawProject{
					Name:     attr(t, "name"),
					Path:     attr(t, "path"),
					Remote:   attr(t, "remote"),
					Revision: attr(t, "revision"),
					URL:      attr(t, "url"),
				}
				if v := attr(t, "sync-c"); v != "" {
					p.SyncC = parseBool(v)
				}
				if p.Name == "" {
					die("<project> without 'name': %s", relpath)
				}
				cur = p
			case "include":
				checkAttrs(m, tag, t)
				sub := attr(t, "name")
				if sub == "" {
					die("<include> without 'name': %s", relpath)
				}
				sm := parseManifestRec(manifestsRoot, sub, visited, parsed)
				mergeManifest(m, sm)
			case "remove-project":
				checkAttrs(m, tag, t)
				name := attr(t, "name")
				if name == "" {
					die("<remove-project> without 'name': %s", relpath)
				}
				for i := range m.Projects { // 文档顺序:只裁已定义的
					if m.Projects[i].Name == name {
						m.Projects[i].Dead = true
					}
				}
			case "archive":
				checkAttrs(m, tag, t)
				a := RawArchive{
					URL:    attr(t, "url"),
					Path:   attr(t, "path"),
					SHA256: strings.ToLower(attr(t, "sha256")),
					Format: attr(t, "format"),
				}
				if a.URL == "" || a.Path == "" {
					die("<archive> needs 'url' and 'path': %s", relpath)
				}
				if v := attr(t, "strip"); v != "" {
					n, err := strconv.Atoi(v)
					if err != nil || n < 0 {
						die("<archive strip=%q> is not a number: %s", v, relpath)
					}
					a.Strip = n
				}
				m.Archives = append(m.Archives, a)
			default:
				m.Ignored["<"+tag+">"] = true
				dec.Skip()
			}
		case xml.EndElement:
			if t.Name.Local == "project" && cur != nil {
				m.Projects = append(m.Projects, *cur)
				cur = nil
			}
		}
	}
	if !sawRoot {
		die("%s is not a repo manifest: no <manifest> root element found", relpath)
	}
	if cur != nil {
		die("unclosed <project> in %s", relpath)
	}

	if len(visited) == 1 { // 顶层:警告 + 同名去重(最后一个定义生效)
		if len(m.Ignored) > 0 {
			keys := make([]string, 0, len(m.Ignored))
			for k := range m.Ignored {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			warn("unsupported manifest features ignored: %s", strings.Join(keys, ", "))
		}
		final := m.Projects[:0]
		last := map[string]int{}
		for i, p := range m.Projects {
			last[p.Name] = i // 同名取最后定义
		}
		for i, p := range m.Projects {
			if i == last[p.Name] && !p.Dead {
				final = append(final, p)
			}
		}
		m.Projects = final
	}
	return m
}

// mergeManifest 把 include 进来的子清单并进来:remote/default 只补缺失键
// (包含者赢),列表追加,remove 标记照单全收(文档顺序已在子解析内生效)。
func mergeManifest(m, sub *Manifest) {
	for k, v := range sub.Remotes {
		if _, ok := m.Remotes[k]; !ok {
			m.Remotes[k] = v
		}
	}
	if m.Default.Remote == "" {
		m.Default.Remote = sub.Default.Remote
	}
	if m.Default.Revision == "" {
		m.Default.Revision = sub.Default.Revision
	}
	if !m.Default.syncCSeen && sub.Default.syncCSeen {
		m.Default.SyncC = sub.Default.SyncC
		m.Default.syncCSeen = true
	}
	m.Projects = append(m.Projects, sub.Projects...)
	m.Archives = append(m.Archives, sub.Archives...)
	for k := range sub.Ignored {
		m.Ignored[k] = true
	}
}

// insidePath target 必须落在 parent 内(realpath,符号链接骗不过)
func insidePath(parent, target string) bool {
	parent = resolveReal(parent)
	target = resolveReal(target)
	rel, err := filepath.Rel(parent, target)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "")
}

func resolveReal(p string) string {
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	// 目标尚不存在:解析最近存在的祖先再拼回去
	dir, base := filepath.Split(filepath.Clean(p))
	if dir != "" {
		return filepath.Join(resolveReal(filepath.Clean(dir)), base)
	}
	return filepath.Clean(p)
}

var _ = fmt.Sprintf // keep imports honest when helpers evolve
