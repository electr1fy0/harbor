//go:build darwin

package capacity

import (
	"errors"
	"math"
	"syscall"
)

type filesystemInfo struct{ total, available, used int64 }

func readFilesystem(path string) (filesystemInfo, error) {
	var info syscall.Statfs_t
	if err := syscall.Statfs(path, &info); err != nil {
		return filesystemInfo{}, err
	}
	if info.Bsize <= 0 {
		return filesystemInfo{}, errors.New("filesystem capacity unavailable")
	}
	blockSize := uint64(info.Bsize)
	totalBlocks := uint64(info.Blocks)
	availableBlocks := uint64(info.Bavail)
	freeBlocks := uint64(info.Bfree)
	if totalBlocks > math.MaxInt64/blockSize || availableBlocks > math.MaxInt64/blockSize || freeBlocks > math.MaxInt64/blockSize {
		return filesystemInfo{}, errors.New("filesystem capacity unavailable")
	}
	total := int64(totalBlocks * blockSize)
	available := int64(availableBlocks * blockSize)
	used := int64((totalBlocks - min(totalBlocks, freeBlocks)) * blockSize)
	if available > total {
		available = total
	}
	return filesystemInfo{total: total, available: available, used: used}, nil
}
