package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/google/renameio"
	"golang.org/x/xerrors"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
	"google.golang.org/protobuf/encoding/prototext"
)

const bumpHelp = `distri bump [-flags] [package]

Increase the distri revision number of the specified packages and their affected
reverse dependencies.

Example:
  % distri bump i3status
`

type versionIncrement struct {
	pkg     string
	current string
	new     string
}

func (i *versionIncrement) Perform() error {
	fn := filepath.Join(env.DistriRoot, "pkgs", i.pkg, "build.textproto")
	b, err := ioutil.ReadFile(fn)
	if err != nil {
		return err
	}
	// TODO: programmatically modify textproto
	lines := strings.Split(string(b), "\n")
	rewritten := make([]string, len(lines))
	for idx, line := range lines {
		rewritten[idx] = strings.ReplaceAll(line, `version: "`+i.current+`"`, `version: "`+i.new+`"`)
	}
	return renameio.WriteFile(fn, []byte(strings.Join(rewritten, "\n")), 0644)
}

func incrementVersion(current string) string {
	v := distri.ParseVersion(current)
	v.DistriRevision++
	return fmt.Sprintf("%s-%d", v.Upstream, v.DistriRevision)
}

func bumpAll(write bool) ([]versionIncrement, error) {
	d, err := os.Open(filepath.Join(env.DistriRoot, "pkgs"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	d.Close()
	var inc []versionIncrement
	for _, name := range names {
		fn := filepath.Join(env.DistriRoot, "pkgs", name, "build.textproto")
		build, err := pb.ReadBuildFile(fn)
		if err != nil {
			return nil, err
		}

		inc = append(inc, versionIncrement{
			pkg:     name,
			current: build.GetVersion(),
			new:     incrementVersion(build.GetVersion()),
		})
	}
	return inc, nil
}

type bumpctx struct {
	graph      *simple.DirectedGraph
	arch       string
	cnt        int64
	byFullname map[string]*bumpnode // e.g. gcc-amd64-8.2.0
	byPkg      map[string]*bumpnode // e.g. gcc
	srcCache   map[string]string    // pkg → src, e.g. gcc-libs → gcc
	globCache  map[string][]string  // TODO: remove in favor of a single pass through env.DefaultRepo
	bumped     map[string]bool
}

type bumpnode struct {
	id int64

	pkg      string // e.g. make
	version  string // TODO: remove fullname
	fullname string // package and version, e.g. make-4.2.1
	deps     []string
}

func (n *bumpnode) ID() int64 { return n.id }

func (b *bumpctx) srcOf(pkg string) (string, error) {
	if src, ok := b.srcCache[pkg]; ok {
		return src, nil
	}
	m, err := pb.ReadMetaFile(filepath.Join(env.DefaultRepo, pkg+".meta.textproto"))
	if err != nil {
		return "", err
	}
	src := pkg
	if sp := m.GetSourcePkg(); sp != "" {
		src = sp
	}
	b.srcCache[pkg] = src
	return src, nil
}

func (b *bumpctx) addPkg(pkg string) error {
	buildTextprotoPath := filepath.Join(env.DistriRoot, "pkgs", pkg, "build.textproto")
	c, err := ioutil.ReadFile(buildTextprotoPath)
	if err != nil {
		return err
	}
	var buildProto pb.Build
	if err := prototext.Unmarshal(c, &buildProto); err != nil {
		return fmt.Errorf("%v: %v", pkg, err)
	}
	fullname := pkg + "-" + b.arch + "-" + buildProto.GetVersion()
	if _, ok := b.byFullname[fullname]; ok {
		return nil // already added
	}
	//log.Printf("adding %s", pkg)
	b.cnt++
	n := &bumpnode{
		id:       b.cnt,
		pkg:      pkg,
		fullname: fullname,
		version:  buildProto.GetVersion(),
	}
	b.byPkg[n.pkg] = n
	b.byPkg[n.pkg+"-"+b.arch] = n
	b.byFullname[n.fullname] = n
	b.graph.AddNode(n)

	{
		deps := buildProto.GetDep()
		bld := &build.Ctx{
			Arch: "amd64", // TODO
			Repo: env.DefaultRepo,
		}
		deps = append(deps, bld.Builderdeps(&buildProto)...)
		deps = append(deps, buildProto.GetRuntimeDep()...)
		srcs := make([]string, 0, len(deps))
		for _, r := range deps {
			g, ok := b.globCache[r]
			if !ok {
				var err error
				g, err = bld.Glob(env.DefaultRepo, []string{r})
				if err != nil || len(g) == 0 {
					continue // build.textproto present, but no build artifact
				}
				b.globCache[r] = g
			}
			src, err := b.srcOf(g[0])
			if err != nil {
				return err
			}
			srcs = append(srcs, src)
		}
		//log.Printf("srcs = %v", srcs)
		n.deps = srcs
	}

	return nil
}

func newBumpctx() (*bumpctx, error) {
	b := &bumpctx{
		// TODO: use simple.NewDirectedMatrix instead?
		graph:      simple.NewDirectedGraph(),
		arch:       "amd64", // TODO: configurable / auto-detect
		byFullname: make(map[string]*bumpnode),
		byPkg:      make(map[string]*bumpnode),
		srcCache:   make(map[string]string),
		globCache:  make(map[string][]string),
		bumped:     make(map[string]bool),
	}
	d, err := os.Open(filepath.Join(env.DistriRoot, "pkgs"))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	d.Close()
	//var inc []versionIncrement
	for _, pkg := range names {
		if err := b.addPkg(pkg); err != nil {
			return nil, err
		}
	}
	// All nodes added, add edges between nodes
	nodes := b.graph.Nodes()
	for nodes.Next() {
		n := nodes.Node().(*bumpnode)
		for _, dep := range n.deps {
			if dep == n.fullname ||
				dep == n.pkg+"-"+b.arch ||
				dep == n.pkg {
				continue // skip adding self edges
			}
			if d, ok := b.byFullname[dep]; ok {
				b.graph.SetEdge(b.graph.NewEdge(n, d))
			}
			if d, ok := b.byPkg[dep]; ok {
				b.graph.SetEdge(b.graph.NewEdge(n, d))
			}
		}
	}

	// Break cycles
	if _, err := topo.Sort(b.graph); err != nil {
		uo, ok := err.(topo.Unorderable)
		if !ok {
			return nil, err
		}
		for _, component := range uo { // cyclic component
			//log.Printf("uo %d", idx)
			for _, n := range component {
				//log.Printf("  bootstrap %v", n.(*bumpnode).pkg)
				from := b.graph.From(n.ID())
				for from.Next() {
					b.graph.RemoveEdge(n.ID(), from.Node().ID())
				}
			}
		}
		if _, err := topo.Sort(b.graph); err != nil {
			return nil, xerrors.Errorf("could not break cycles: %v", err)
		}
	}

	return b, nil
}

func (b *bumpctx) bumpPkg(pkg string) ([]versionIncrement, error) {
	if b.bumped[pkg] {
		return nil, nil
	}
	n := b.byPkg[pkg]
	nodes := b.graph.To(n.id)
	inc := []versionIncrement{
		versionIncrement{
			pkg:     n.pkg,
			current: n.version,
			new:     incrementVersion(n.version),
		},
	}
	b.bumped[pkg] = true
	for nodes.Next() {
		n := nodes.Node().(*bumpnode)
		tmp, err := b.bumpPkg(n.pkg)
		if err != nil {
			return nil, err
		}
		inc = append(inc, tmp...)
	}

	return inc, nil
}

func bump(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("bump", flag.ExitOnError)
	var (
		all   = fset.Bool("all", false, "bump all packages")
		write = fset.Bool("w", false, "write changes (default is a dry run)")
	)
	fset.Usage = usage(fset, bumpHelp)
	fset.Parse(args)

	var inc []versionIncrement
	if *all {
		var err error
		inc, err = bumpAll(*write)
		if err != nil {
			return err
		}
	} else {
		if fset.NArg() == 0 {
			fmt.Fprintln(os.Stderr, "syntax: bump [-flags] <package> [<package>...]")
			os.Exit(2)
		}

		b, err := newBumpctx()
		if err != nil {
			return fmt.Errorf("bumpctx: %v", err)
		}
		for _, arg := range fset.Args() {
			tmp, err := b.bumpPkg(arg)
			if err != nil {
				return fmt.Errorf("bump(%v): %v", arg, err)
			}
			inc = append(inc, tmp...)
		}
	}
	if *write {
		for _, i := range inc {
			if err := i.Perform(); err != nil {
				return err
			}
			log.Printf("bumped %s from %s to %s", i.pkg, i.current, i.new)
		}
		return nil
	} else {
		for _, i := range inc {
			log.Printf("bump package %s from %s to %s", i.pkg, i.current, i.new)
		}
	}

	return nil
}
