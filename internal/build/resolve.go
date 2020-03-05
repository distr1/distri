package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/pb"
)

func resolve1(imgDir string, pkg string, seen map[string]bool, prune string) ([]string, error) {
	if distri.ParseVersion(pkg).Pkg == prune {
		return nil, nil
	}
	const ext = ".meta.textproto"
	resolved := []string{pkg}
	fn := filepath.Join(imgDir, pkg+ext)
	if target, err := os.Readlink(fn); err == nil {
		resolved = []string{strings.TrimSuffix(filepath.Base(target), ext)}
	}
	meta, err := pb.ReadMetaFile(filepath.Join(imgDir, pkg+".meta.textproto"))
	if err != nil {
		return nil, err
	}
	for _, dep := range meta.GetRuntimeDep() {
		if dep == pkg {
			continue // skip circular dependencies, e.g. gcc depends on itself
		}
		if seen[dep] {
			continue
		}
		seen[dep] = true
		// TODO: remove this recursion: runtime deps are stored fully resolved
		r, err := resolve1(imgDir, dep, seen, prune)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, r...)
	}
	return resolved, nil
}

// resolve returns the transitive closure of runtime dependencies for the
// specified packages.
//
// E.g., if iptables depends on libnftnl, which depends on libmnl,
// resolve("iptables") will return ["iptables", "libnftnl", "libmnl"].
//
// TODO: remove this recursion: runtime deps are stored fully resolved
func Resolve(imgDir string, pkgs []string, prune string) ([]string, error) {
	var resolved []string
	seen := make(map[string]bool)
	for _, pkg := range pkgs {
		if seen[pkg] {
			continue // a recursive call might have found this package already
		}
		seen[pkg] = true
		r, err := resolve1(imgDir, pkg, seen, prune)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, r...)
	}
	return resolved, nil
}

// GlobAndResolve is a convenience function to call Glob followed by Resolve.
func (b *Ctx) GlobAndResolve(repo string, deps []string, prune string) ([]string, error) {
	globbed, err := b.Glob(repo, deps)
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}
	resolved, err := Resolve(repo, globbed, prune)
	if err != nil {
		return nil, fmt.Errorf("resolve: %w", err)
	}
	return resolved, nil
}
