package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type luksHeader struct {
	Magic         [6]uint8
	Version       uint16
	CipherName    [32]byte
	CipherMode    [32]byte
	HashSpec      [32]uint8
	PayloadOffset uint32
	KeyBytes      uint32
	MkDigest      [20]byte
	MkDigestSalt  [32]byte
	MkDigestIter  uint32
	UUID          [40]byte
}

var errNotFound = errors.New("not found")

func probeLUKS(r io.ReadSeeker) (string, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var hdr luksHeader
	if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
		return "", err
	}
	wantMagic := append([]byte("LUKS"), 0xba, 0xbe)
	if !bytes.Equal(hdr.Magic[:], wantMagic) {
		return "", errNotFound
	}
	return string(hdr.UUID[:bytes.IndexByte(hdr.UUID[:], 0)]), nil
}

func probeLVM(r io.ReadSeeker) error {
	buf := make([]byte, 8)
	// The LVM physical volume label can be stored in any of the first 4
	// sectors:
	for sector := 0; sector < 4; sector++ {
		if _, err := r.Seek(int64(sector)*512, io.SeekStart); err != nil {
			return err
		}
		if _, err := r.Read(buf); err != nil {
			return err
		}
		wantMagic := []byte("LABELONE") // LVM physical volume signature
		if bytes.Equal(buf, wantMagic) {
			return nil
		}
	}
	return errNotFound
}

func blkid(r io.ReadSeeker) (string, error) {
	uuid, err := probeLUKS(r)
	if err != nil && err != errNotFound {
		return "", err
	}
	if err == nil {
		return uuid, nil
	}

	// probe ext4
	const extSuperblockOffset = 0x400
	if _, err := r.Seek(extSuperblockOffset, io.SeekStart); err != nil {
		return "", err
	}
	var sb ext2SuperBlock
	if err := binary.Read(r, binary.LittleEndian, &sb); err != nil {
		return "", err
	}
	if sb.Magic != 0xef53 {
		return "", fmt.Errorf("no ext4 superblock found")
	}
	buf := sb.UUID
	return fmt.Sprintf(
		"%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		buf[0], buf[1], buf[2], buf[3],
		buf[4], buf[5],
		buf[6], buf[7],
		buf[8], buf[9],
		buf[10], buf[11], buf[12], buf[13], buf[14], buf[15]), nil
}

type ext2SuperBlock struct {
	InodesCount         uint32
	BlocksCount         uint32
	ReservedBlocksCount uint32
	FreeBlocksCount     uint32
	FreeInodesCount     uint32
	FirstDataBlock      uint32
	LogBlockSize        uint32
	LogFragSize         int32
	BlocksPerGroup      uint32
	FragsPerGroup       uint32
	InodesPerGroup      uint32
	Mtime               uint32
	Wtime               uint32
	MountCount          uint16
	MaxMountCount       int16
	Magic               uint16
	State               uint16
	Errors              uint16
	MinorRevLevel       uint16
	LastCheck           uint32
	CheckInterval       uint32
	CreatorOS           uint32
	RevLevel            uint32
	DefResuid           uint16
	DefResgid           uint16
	FirstIno            uint32
	InodeSize           uint16
	BlockGroupNr        uint16
	FeatureCompat       uint32
	FeatureIncompat     uint32
	FeatureRoCompat     uint32
	UUID                [16]uint8

	// remaining fields elided (irrelevant for probing)
}
