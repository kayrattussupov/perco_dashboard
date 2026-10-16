//go:build !windows

package engine

func diskFree(string) int64 { return -1 }
