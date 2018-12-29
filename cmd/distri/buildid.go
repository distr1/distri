package main

import (
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

// from binutils/include/elf/common.h
const NT_GNU_BUILD_ID = 3

// from go/src/cmd/internal/buildid
func readAligned4(r io.Reader, sz int32) ([]byte, error) {
	full := (sz + 3) &^ 3
	data := make([]byte, full)
	_, err := io.ReadFull(r, data)
	if err != nil {
		return nil, err
	}
	data = data[:sz]
	return data, nil
}

// based on go/src/cmd/internal/buildid.ReadELFNote
func readBuildid(filename string) (string, error) {
	f, err := elf.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sect := f.Section(".note.gnu.build-id")
	if got, want := sect.Type, elf.SHT_NOTE; got != want {
		return "", fmt.Errorf("ELF note type = %v, want %v", got, want)
	}
	r := sect.Open()
	var meta struct {
		Namesize, Descsize, NoteType int32
	}
	if err := binary.Read(r, f.ByteOrder, &meta); err != nil {
		return "", fmt.Errorf("read namesize failed: %v", err)
	}
	if got, want := meta.NoteType, int32(NT_GNU_BUILD_ID); got != want {
		return "", fmt.Errorf("note type = %v, want %v", got, want)
	}
	noteName, err := readAligned4(r, meta.Namesize)
	if err != nil {
		return "", fmt.Errorf("read name failed: %v", err)
	}
	if got, want := string(noteName), "GNU\x00"; got != want {
		return "", fmt.Errorf("note name = %q, want %q", got, want)
	}
	desc, err := readAligned4(r, meta.Descsize)
	if err != nil {
		return "", fmt.Errorf("read desc failed: %v", err)
	}
	if len(desc) < 2 {
		return "", fmt.Errorf("desc too short: %d, want ≥ 2", len(desc))
	}
	return hex.EncodeToString(desc), nil
}
