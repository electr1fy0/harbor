//go:build !linux && !darwin

package capacity

import "errors"

type filesystemInfo struct{ total, available, used int64 }

func readFilesystem(string) (filesystemInfo, error) {
	return filesystemInfo{}, errors.New("filesystem capacity is unsupported on this platform")
}
