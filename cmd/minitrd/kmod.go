package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type moduleEntry struct {
	Path string
	Deps []string
}

type moduleAlias struct {
	Pattern string
	Module  string
}

var (
	aliases []moduleAlias

	// keyed by strings.TrimSuffix(filepath.Base(module), ".ko")
	deps = make(map[string]moduleEntry)
)

func loadModule(module string) error {
	if debug {
		fmt.Printf("loadModule(%s)\n", module)
	}
	me, ok := deps[module]
	if !ok {
		return fmt.Errorf("module %q not found", module)
	}
	all := make([]string, 0, len(me.Deps)+1)
	all = append(all, me.Deps...)
	all = append(all, me.Path)
	for _, mod := range all {
		if debug {
			fmt.Printf("  initModule(%v)\n", mod)
		}
		f, err := os.Open(filepath.Join("/lib/modules", release, mod))
		if err != nil {
			if os.IsNotExist(err) {
				// The initrd intentionally only contains the kernel modules
				// necessary for mounting the root file system.
				return nil
			}
			return err
		}
		if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil {
			if err != unix.EEXIST &&
				err != unix.EBUSY &&
				err != unix.ENODEV &&
				err != unix.ENOENT {
				f.Close()
				return fmt.Errorf("FinitModule(%v): %v", mod, err)
			}
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "minitrd: %v\n", err)
		}
	}
	return nil
}

func loadModalias(modalias string) error {
	if debug {
		fmt.Printf("loadModalias(%v)\n", modalias)
	}
	for _, a := range aliases {
		matched, err := filepath.Match(a.Pattern, modalias)
		if err != nil {
			return err
		}
		if !matched {
			continue
		}
		if err := loadModule(a.Module); err != nil {
			return err
		}
	}
	return nil
}

var release = func() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		fmt.Fprintf(os.Stderr, "minitrd: %v\n", err)
		os.Exit(1)
	}
	return string(uts.Release[:bytes.IndexByte(uts.Release[:], 0)])
}()

func parseAliases() error {
	// TODO(later): evaluate using binary modules.alias
	f, err := os.Open(filepath.Join("/lib/modules", release, "modules.alias"))
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "alias ") {
			continue // also skips comments
		}
		line = strings.TrimPrefix(line, "alias ")
		// some aliases (e.g. acerhdf) include a space in their pattern
		idx := strings.LastIndexByte(line, ' ')
		if idx == -1 {
			log.Printf("BUG: modules.alias line has no space: %q", line)
			continue
		}
		aliases = append(aliases, moduleAlias{
			Pattern: line[:idx],
			Module:  line[idx+1:],
		})
	}
	return scanner.Err()
}

func parseDeps() error {
	f, err := os.Open(filepath.Join("/lib/modules", release, "modules.dep"))
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), " ")
		base := strings.TrimSuffix(filepath.Base(parts[0]), ".ko:")
		// Normalize dashes to underscores: some aliases refer to
		// e.g. acpi_cpufreq, which is present as acpi-cpufreq.ko.
		base = strings.ReplaceAll(base, "-", "_")
		deps[base] = moduleEntry{
			Path: strings.TrimSuffix(parts[0], ":"),
			Deps: parts[1:],
		}
	}
	return scanner.Err()
}
