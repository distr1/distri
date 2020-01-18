package main

import (
	"os"
	"testing"
)

func TestBlkidLUKS(t *testing.T) {
	f, err := os.Open("testdata/luks_header.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := "0d7b09a9-8928-4451-8037-21f7a329fed8"
	got, err := blkid(f)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("blkid(luks_header.bin) = %v, want %v", got, want)
	}
}

func TestBlkidExt4(t *testing.T) {
	f, err := os.Open("testdata/ext4_superblock.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := "1fa04de7-30a9-4183-93e9-1b0061567121"
	got, err := blkid(f)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("blkid(ext4_superblock.bin) = %v, want %v", got, want)
	}
}
