package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestPartuuid(t *testing.T) {
	lsblk := exec.Command("lsblk",
		"--json",
		"--output=name,uuid,partuuid,path")
	lsblk.Stderr = os.Stderr
	b, err := lsblk.Output()
	if err != nil {
		t.Fatal(err)
	}
	type blockdev struct {
		Name     string      `json:"name"`
		UUID     *string     `json:"uuid"`
		PartUUID *string     `json:"partuuid"`
		Path     string      `json:"path"`
		Children []*blockdev `json:"children"`
	}
	var lsblkOut struct {
		Blockdevices []*blockdev `json:"blockdevices"`
	}
	if err := json.Unmarshal(b, &lsblkOut); err != nil {
		t.Fatal(err)
	}
	var test func(dev *blockdev)
	test = func(dev *blockdev) {
		if dev.UUID != nil {
			t.Run("fs/"+dev.Name, func(t *testing.T) {
				got, err := uuid(dev.Path, "fs")
				if err != nil {
					t.Fatal(err)
				}
				want := *dev.UUID
				if got != want {
					t.Errorf("unexpected uuid: got %q, want %q", got, want)
				}
			})
		}
		if dev.PartUUID != nil {
			t.Run("part/"+dev.Name, func(t *testing.T) {
				got, err := uuid(dev.Path, "part")
				if err != nil {
					t.Fatal(err)
				}
				want := *dev.PartUUID
				if got != want {
					t.Errorf("unexpected uuid: got %q, want %q", got, want)
				}
			})
		}
		for _, c := range dev.Children {
			test(c)
		}
	}
	for _, d := range lsblkOut.Blockdevices {
		test(d)
	}
}
