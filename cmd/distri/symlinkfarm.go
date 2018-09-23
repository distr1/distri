package main

import (
	"io/ioutil"
	"os"
	"path/filepath"
)

// root is a path to the ro/ directory
// dir is the directory to create symlinks for (e.g. bin, or lib/systemd/system)
func symlinkfarm(root, pkg, dir string) error {
	// Link <root>/<pkg>-<version>/bin/ entries to <root>/bin:
	dest := filepath.Join(root, filepath.Base(dir))
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	binDir := filepath.Join(root, pkg, dir)
	fis, err := ioutil.ReadDir(binDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // e.g. package does not ship systemd unit files
		}
		return err
	}
	for _, fi := range fis {
		oldname := filepath.Join(binDir, fi.Name())
		newname := filepath.Join(dest, fi.Name())
		tmp, err := ioutil.TempFile(filepath.Dir(newname), "distri")
		if err != nil {
			return err
		}
		tmp.Close()
		if err := os.Remove(tmp.Name()); err != nil {
			return err
		}
		rel, err := filepath.Rel(dest, oldname)
		if err != nil {
			return err
		}
		if err := os.Symlink(rel, tmp.Name()); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), newname); err != nil {
			return err
		}
	}
	return nil
}
