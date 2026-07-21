//go:build windows

package config

import (
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openConfigNoFollow(path string) (*os.File, error) {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("encode config path: %w", err)
	}
	handle, err := windows.CreateFile(
		path16,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open config without following final reparse point: %w", err)
	}
	return os.NewFile(uintptr(handle), path), nil
}

func pathEntryIdentity(_ string, info fs.FileInfo) (string, bool) {
	return fmt.Sprintf("windows-path:%d:%d:%d", info.Size(), info.ModTime().UnixNano(), info.Mode()), true
}

func openedFileIdentity(file *os.File, _ fs.FileInfo) (string, bool) {
	type fileIDInfo struct {
		VolumeSerialNumber uint64
		FileID             [16]byte
	}
	var stable fileIDInfo
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&stable)),
		uint32(unsafe.Sizeof(stable)),
	); err == nil {
		return fmt.Sprintf("windows:%d:%s", stable.VolumeSerialNumber, hex.EncodeToString(stable.FileID[:])), true
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", false
	}
	return fmt.Sprintf("windows:%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), true
}
