package engine

import "golang.org/x/sys/windows"

// diskFree returns the free bytes on the disk holding path, or -1 when unknown.
func diskFree(path string) int64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return -1
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return -1
	}
	return int64(free)
}
