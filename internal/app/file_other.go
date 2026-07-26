//go:build !windows

package app

import (
	"io"
	"os"
)

func copyFileOptimized(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func markSparse(*os.File) bool {
	return false
}

func punchZeroRange(*os.File, int64, int64) bool {
	return false
}
