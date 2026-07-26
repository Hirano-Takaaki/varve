//go:build windows

package app

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	fsctlSetSparse = 0x000900c4
	fsctlSetZero   = 0x000980c8
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procCopyFileW       = kernel32.NewProc("CopyFileW")
	procDeviceIoControl = kernel32.NewProc("DeviceIoControl")
)

type fileZeroDataInformation struct {
	fileOffset      int64
	beyondFinalZero int64
}

func copyFileOptimized(source, destination string) error {
	sourcePtr, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPtr, err := syscall.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	ok, _, callErr := procCopyFileW.Call(
		uintptr(unsafe.Pointer(sourcePtr)),
		uintptr(unsafe.Pointer(destinationPtr)),
		1,
	)
	if ok == 0 {
		if callErr == syscall.Errno(0) {
			return syscall.EINVAL
		}
		return callErr
	}
	return nil
}

func markSparse(file *os.File) bool {
	var returned uint32
	ok, _, _ := procDeviceIoControl.Call(
		file.Fd(),
		fsctlSetSparse,
		0,
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&returned)),
		0,
	)
	return ok != 0
}

func punchZeroRange(file *os.File, offset, size int64) bool {
	info := fileZeroDataInformation{
		fileOffset:      offset,
		beyondFinalZero: offset + size,
	}
	var returned uint32
	ok, _, _ := procDeviceIoControl.Call(
		file.Fd(),
		fsctlSetZero,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
		0,
		0,
		uintptr(unsafe.Pointer(&returned)),
		0,
	)
	return ok != 0
}
