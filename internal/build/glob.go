package build

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/distr1/distri"
)

var globCache = struct {
	sync.Mutex
	C map[string]string
}{C: make(map[string]string)}

func (b *Ctx) Glob1(imgDir, pkg string) (string, error) {
	key := imgDir + "/" + pkg
	globCache.Lock()
	globbed, ok := globCache.C[key]
	globCache.Unlock()
	if ok {
		return globbed, nil
	}
	if st, err := os.Lstat(filepath.Join(imgDir, pkg+".meta.textproto")); err == nil && st.Mode().IsRegular() {
		return pkg, nil // pkg already contains the version
	}
	pkgPattern := pkg
	if suffix, ok := distri.HasArchSuffix(pkg); !ok {
		pkgPattern = pkgPattern + "-" + b.Arch
	} else {
		pkg = strings.TrimSuffix(pkg, "-"+suffix)
	}

	pattern := filepath.Join(imgDir, pkgPattern+"-*.meta.textproto")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, m := range matches {
		if st, err := os.Lstat(m); err != nil || !st.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, strings.TrimSuffix(filepath.Base(m), ".meta.textproto"))
	}
	if len(candidates) > 1 {
		// default to the most recent package revision. If building against an
		// older version is desired, that version must be specified explicitly.
		sort.Slice(candidates, func(i, j int) bool {
			return distri.PackageRevisionLess(candidates[i], candidates[j])
		})
		globbed := candidates[len(candidates)-1]
		globCache.Lock()
		globCache.C[key] = globbed
		globCache.Unlock()
		return globbed, nil
	}
	if len(candidates) == 0 {
		if !b.Hermetic {
			// no package found, fall back to host tools in non-hermetic mode
			return "", nil
		}
		return "", fmt.Errorf("package %q not found (pattern %s)", pkg, pattern)
	}
	globbed = candidates[0]
	globCache.Lock()
	globCache.C[key] = globbed
	globCache.Unlock()
	return globbed, nil
}

func (b *Ctx) Glob(imgDir string, pkgs []string) ([]string, error) {
	globbed := make([]string, 0, len(pkgs))
	for _, pkg := range pkgs {
		tmp, err := b.Glob1(imgDir, pkg)
		if err != nil {
			return nil, err
		}
		if tmp == "" {
			continue
		}
		globbed = append(globbed, tmp)
	}
	return globbed, nil
}
