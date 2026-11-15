// Package version 保存可通过 go build -ldflags 注入的构建信息。
package version

import "fmt"

var (
	// Version 是发布版本，未通过 -ldflags -X 注入时为 dev。
	Version = "dev"
	// Commit 是构建对应的源码提交标识，无 Git 信息时为 none。
	Commit = "none"
	// BuildTime 是 UTC 构建时间，未注入时为 unknown。
	BuildTime = "unknown"
)

// Info 是 version 命令的稳定输出结构。
type Info struct {
	Version   string
	Commit    string
	BuildTime string
}

// Current 读取进程构建时注入的版本信息，不访问 Git 或外部服务。
func Current() Info {
	return Info{Version: Version, Commit: Commit, BuildTime: BuildTime}
}

// String 返回供人和脚本稳定读取的三行纯文本版本信息。
func (i Info) String() string {
	return fmt.Sprintf("version: %s\ncommit: %s\nbuild_time: %s\n", i.Version, i.Commit, i.BuildTime)
}
