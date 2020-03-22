package build

import (
	"debug/dwarf"
	"debug/elf"
	"path/filepath"
	"strings"
)

func dwarfPaths(fn string) ([]string, error) {
	f, err := elf.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dwf, err := f.DWARF()
	if err != nil {
		return nil, err
	}

	var paths []string
	dr := dwf.Reader()
	for {
		ent, err := dr.Next()
		if err != nil {
			return nil, err
		}
		if ent == nil {
			break
		}
		if ent.Tag != dwarf.TagCompileUnit {
			dr.SkipChildren()
			continue
		}
		if ent.Val(dwarf.AttrName) == nil {
			continue
		}
		name := ent.Val(dwarf.AttrName).(string)
		var dir string
		if v := ent.Val(dwarf.AttrCompDir); v != nil {
			dir, _ = v.(string)
		}
		full := name
		if !strings.HasPrefix(full, "/") {
			full = filepath.Join(dir, full)
		}
		paths = append(paths, full)
	}
	return paths, nil
}
