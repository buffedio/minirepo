package main

import (
	"runtime/debug"
	"testing"
)

// TestVersionFromBuildInfo 直接喂 BuildInfo 做定向断言:
// version.go 的回退链每一档都有用例,删掉任何一档都会红。
func TestVersionFromBuildInfo(t *testing.T) {
	cases := []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{"nil 兜底为空", nil, ""},
		{"空 build info 兜底为空", &debug.BuildInfo{}, ""},
		{"go install 模块版本", &debug.BuildInfo{
			Main: debug.Module{Path: "github.com/buffedio/minirepo", Version: "v0.12.0"},
		}, "v0.12.0"},
		{"伪版本如实返回", &debug.BuildInfo{
			Main: debug.Module{Version: "v0.0.0-20260924120000-abcdef123456"},
		}, "v0.0.0-20260924120000-abcdef123456"},
		{"devel 不当版本,回落 VCS", &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1234567890abcdef"},
			},
		}, "1234567"},
		{"空模块版本回落 VCS", &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1234567890abcdef"},
			},
		}, "1234567"},
		{"脏工作区追加 dirty", &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "1234567890abcdef"},
				{Key: "vcs.modified", Value: "true"},
			},
		}, "1234567-dirty"},
		{"revision 不足 7 位不算", &debug.BuildInfo{
			Main:     debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}},
		}, ""},
	}
	for _, c := range cases {
		if got := versionFromBuildInfo(c.bi); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestVersionStringPrefersLDFLAGS: ldflags 注入必须压过 build info
// (正式构建的 git describe 串是唯一权威)。
func TestVersionStringPrefersLDFLAGS(t *testing.T) {
	old := version
	version = "v9.9.9-2-gabcdef0-dirty"
	defer func() { version = old }()
	if got := versionString(); got != "v9.9.9-2-gabcdef0-dirty" {
		t.Errorf("ldflags must win over build info, got %q", got)
	}
}
