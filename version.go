package main

import (
	"fmt"
	"runtime/debug"
)

// version 由构建时 ldflags 注入（见 Makefile）：
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// 未注入时回退到 debug.ReadBuildInfo 的 VCS 信息，最后兜底 "dev"。
var version = ""

// versionString 返回可展示的版本号
func versionString() string {
	// 1. ldflags 注入（正式构建）
	if version != "" {
		return version
	}
	// 2. go build 工具链记录的 VCS 信息（Go 1.18+，git 仓内直接 go build 时可用）
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, kv := range info.Settings {
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
	}
	// 3. 兜底
	return "dev"
}

// versionDetail 返回带构建信息的多行版本描述
func versionDetail() string {
	s := fmt.Sprintf("minirepo %s\n", versionString())
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, kv := range info.Settings {
			switch kv.Key {
			case "vcs.time":
				s += fmt.Sprintf("  commit time: %s\n", kv.Value)
			case "vcs.modified":
				if kv.Value == "true" {
					s += "  working tree: dirty\n"
				}
			case "GOOS", "GOARCH":
				s += fmt.Sprintf("  %s: %s\n", kv.Key, kv.Value)
			}
		}
	}
	return s
}
