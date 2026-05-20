package transcode

import "fmt"

var (
	BuildVersion = "dev"
	BuildCommit  = "no-vcs"
	BuildTime    = "unknown"
)

func BuildSummary(binary string) string {
	return fmt.Sprintf("%s version=%s commit=%s built=%s", binary, BuildVersion, BuildCommit, BuildTime)
}
