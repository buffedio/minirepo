package main

import (
	"runtime/debug"
)

// version 由构建时 ldflags 注入（见 Makefile）：
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// 未注入时回退到 debug.ReadBuildInfo：先取模块版本（go install 装出的没有
// ldflags、也没有 VCS 信息，只有它），再取 VCS 短 hash，最后兜底 "dev"。
var version = ""

// versionString 返回可展示的版本号（约定：单行裸 token，脚本可直接比较）。
func versionString() string {
	// 1. ldflags 注入（正式构建）
	if version != "" {
		return version
	}
	// 2. build info：模块版本或 VCS 短 hash
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := versionFromBuildInfo(bi); v != "" {
			return v
		}
	}
	// 3. 兜底
	return "dev"
}

// versionFromBuildInfo 从 build info 提取版本串，取不到返回 ""。
// 真实模块版本优先（go install pkg@vX 的产物只有它），
// 其次 git 短 hash（仓内裸 go build 的产物）。
func versionFromBuildInfo(bi *debug.BuildInfo) string {
	if bi == nil {
		return ""
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, dirty string
	for _, kv := range bi.Settings {
		switch kv.Key {
		case "vcs.revision":
			if len(kv.Value) >= 7 {
				rev = kv.Value[:7]
			}
		case "vcs.modified":
			if kv.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev != "" {
		return rev + dirty
	}
	return ""
}
