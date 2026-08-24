//go:build unix

package operations

import "golang.org/x/sys/unix"

type DiskHealth struct {
	Known       bool    `json:"known"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

func readDiskHealth() DiskHealth {
	var s unix.Statfs_t
	if unix.Statfs(".", &s) != nil || s.Blocks == 0 {
		return DiskHealth{}
	}
	free := s.Bavail * uint64(s.Bsize)
	used := float64(s.Blocks-s.Bfree) / float64(s.Blocks) * 100
	return DiskHealth{Known: true, FreeBytes: free, UsedPercent: used}
}
