package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

const CAP_SETFCAP = 31 // from linux/include/uapi/linux/capability.h

type capHeader struct {
	version uint32
	pid     int
}

type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

type caps struct {
	hdr  capHeader
	data [2]capData
}

func getCaps() (caps, error) {
	var c caps

	// Get capability version
	if _, _, errno := syscall.Syscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&c.hdr)), uintptr(unsafe.Pointer(nil)), 0); errno != 0 {
		return c, fmt.Errorf("SYS_CAPGET: %v", errno)
	}

	// Get current capabilities
	if _, _, errno := syscall.Syscall(syscall.SYS_CAPGET, uintptr(unsafe.Pointer(&c.hdr)), uintptr(unsafe.Pointer(&c.data[0])), 0); errno != 0 {
		return c, fmt.Errorf("SYS_CAPGET: %v", errno)
	}

	return c, nil
}

func setCaps() error {
	caps, err := getCaps()
	if err != nil {
		return err
	}

	// Add CAP_SYS_TIME to the permitted and inheritable capability mask,
	// otherwise we will not be able to add it to the ambient capability mask.
	caps.data[0].permitted |= 1 << uint(CAP_SETFCAP)
	caps.data[0].inheritable |= 1 << uint(CAP_SETFCAP)

	if _, _, errno := syscall.Syscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&caps.hdr)), uintptr(unsafe.Pointer(&caps.data[0])), 0); errno != 0 {
		return err
	}

	return nil
}
