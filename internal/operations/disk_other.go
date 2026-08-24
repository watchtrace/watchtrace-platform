//go:build !unix

package operations

type DiskHealth struct {
	Known       bool    `json:"known"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

func readDiskHealth() DiskHealth { return DiskHealth{} }
