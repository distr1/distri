package batch

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/distr1/distri"
	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/internal/trace"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

type node struct {
	id int64

	pkg      string // e.g. make
	fullname string // package and version, e.g. make-4.2.1
}

func (n *node) ID() int64 { return n.id }

// Ctx is a batch build context, containing configuration and state.
type Ctx struct {
	// Configuration
	Log             *log.Logger
	DistriRoot      string
	DefaultBuildCtx *build.Ctx
}

func (c *Ctx) Build(ctx context.Context, dryRun, simulate, rebuild bool, jobs int) error {
	c.Log.Printf("distriroot %q", c.DistriRoot)

	// TODO: use simple.NewDirectedMatrix instead?
	g := simple.NewDirectedGraph()

	const arch = "amd64" // TODO: configurable / auto-detect

	std := c.DefaultBuildCtx.Clone()

	pkgsDir := filepath.Join(c.DistriRoot, "pkgs")
	fis, err := ioutil.ReadDir(pkgsDir)
	if err != nil {
		return err
	}
	byFullname := make(map[string]*node) // e.g. gcc-amd64-8.2.0
	byPkg := make(map[string]*node)      // e.g. gcc

	sourceBySplit := make(map[string]string) // from split pkg to source pkg
	for _, fi := range fis {
		src := fi.Name()
		sourceBySplit[src] = src
		sourceBySplit[src+"-"+arch] = src
		buildTextprotoPath := filepath.Join(pkgsDir, src, "build.textproto")
		buildProto, err := pb.ReadBuildFile(buildTextprotoPath)
		if err != nil {
			return err
		}
		for _, pkg := range buildProto.GetSplitPackage() {
			sourceBySplit[pkg.GetName()] = src
			sourceBySplit[pkg.GetName()+"-"+arch] = src
		}
	}

	for idx, fi := range fis {
		pkg := fi.Name()

		// TODO(later): parallelize?
		buildTextprotoPath := filepath.Join(pkgsDir, fi.Name(), "build.textproto")
		buildProto, err := pb.ReadBuildFile(buildTextprotoPath)
		if err != nil {
			return err
		}
		b := c.DefaultBuildCtx.Clone()
		b.Pkg = fi.Name()
		b.PkgDir = filepath.Join(pkgsDir, fi.Name())
		b.Proto = buildProto
		b.GlobHook = func(imgDir, pkg string) (out string, err error) {
			// imgDir is e.g. /home/michael/distri/build/distri/pkg
			src, ok := sourceBySplit[pkg]
			if !ok {
				if arch, ok := distri.HasArchSuffix(pkg); ok {
					pkg = strings.TrimSuffix(pkg, "-"+arch)
				}
				src, ok = sourceBySplit[pkg]
				if !ok {
					return "", fmt.Errorf("package not found!")
				}
			}
			build, err := pb.ReadBuildFile(filepath.Join(imgDir, "../../../pkgs", src, "build.textproto"))
			if err != nil {
				return "", err
			}
			fullName := fmt.Sprintf("%s-%s-%s", src, std.Arch, build.GetVersion())
			if out != fullName {
				return fullName, nil
			}
			return out, nil
		}
		inputDigest, err := b.Digest()
		if err != nil {
			return fmt.Errorf("digest: %w", err)
		}

		fullname := pkg + "-" + arch + "-" + buildProto.GetVersion()
		if !simulate {
			fn := filepath.Join(c.DistriRoot, "build", "distri", "pkg", fullname+".meta.textproto")
			meta, err := pb.ReadMetaFile(fn)
			if err != nil {
				if !os.IsNotExist(err) {
					return err
				}
			}
			if !rebuild && meta.GetInputDigest() == inputDigest {
				continue // package already built
			}
			c.Log.Printf("stale package %s (got input_digest %q, want %q)",
				pkg,
				meta.GetInputDigest(),
				inputDigest)
			// fall-through: stale package
		}

		// TODO: to conserve work, only add nodes which need to be rebuilt
		n := &node{
			id:       int64(idx),
			pkg:      pkg,
			fullname: fullname,
		}
		byPkg[n.pkg] = n
		byPkg[n.pkg+"-"+arch] = n
		byFullname[n.fullname] = n
		g.AddNode(n)
	}

	b := &build.Ctx{
		Arch: "amd64", // TODO
		Repo: env.DefaultRepo,
	}

	// add all constraints: <pkg>-<version> depends on <pkg>-<version>
	for _, n := range byFullname {
		// TODO(later): parallelize?
		c, err := ioutil.ReadFile(filepath.Join(pkgsDir, n.pkg, "build.textproto"))
		if err != nil {
			return err
		}
		var buildProto pb.Build
		if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
			return err
		}
		version := buildProto.GetVersion()

		deps := buildProto.GetDep()
		deps = append(deps, b.Builderdeps(&buildProto)...)
		deps = append(deps, buildProto.GetRuntimeDep()...)

		for _, dep := range deps {
			if dep == n.pkg+"-"+arch+"-"+version ||
				dep == n.pkg+"-"+arch ||
				dep == n.pkg {
				continue // skip adding self edges
			}
			if d, ok := byFullname[dep]; ok {
				g.SetEdge(g.NewEdge(n, d))
			}
			if d, ok := byPkg[dep]; ok {
				g.SetEdge(g.NewEdge(n, d))
			}
			// dependency already built
		}
	}

	// detect cycles and break them

	// strong-set = packages which needed a cycle break
	// 1. build the strong-set once, in any order, with host deps (= remove all deps)
	// 2. build the strong-set again, with the results of the previous compilation
	// 3. build the rest of the packages

	// scc := topo.TarjanSCC(g)
	// log.Printf("%d scc", len(scc))
	// for idx, c := range scc {
	// 	log.Printf("scc %d", idx)
	// 	for _, n := range c {
	// 		log.Printf("  n %v", n)
	// 	}
	// }

	// Break cycles
	if _, err := topo.Sort(g); err != nil {
		uo, ok := err.(topo.Unorderable)
		if !ok {
			return err
		}
		for _, component := range uo { // cyclic component
			//log.Printf("uo %d", idx)
			for _, n := range component {
				c.Log.Printf("  bootstrap %v", n.(*node).pkg)
				from := g.From(n.ID())
				for from.Next() {
					g.RemoveEdge(n.ID(), from.Node().ID())
				}
			}
		}
		if _, err := topo.Sort(g); err != nil {
			return xerrors.Errorf("could not break cycles: %v", err)
		}
	}

	if dryRun {
		if g.Nodes() == nil {
			c.Log.Printf("build 0 pkg")
			return nil
		}
		c.Log.Printf("build %d pkg", g.Nodes().Len())
		for it := g.Nodes(); it.Next(); {
			c.Log.Printf("  build %s", it.Node().(*node).pkg)
		}
		return nil
	}

	logDir, err := ioutil.TempDir("", "distri-batch")
	if err != nil {
		return err
	}
	s := scheduler{
		distriRoot: c.DistriRoot,
		log:        c.Log,
		logDir:     logDir,
		simulate:   simulate,
		workers:    jobs,
		g:          g,
		byFullname: byFullname,
		built:      make(map[string]error),
		status:     make([]string, jobs+1),
	}
	if err := s.run(ctx); err != nil {
		return err
	}

	return nil
}

type buildResult struct {
	node *node
	err  error
}

type scheduler struct {
	distriRoot string
	log        *log.Logger
	logDir     string
	simulate   bool
	workers    int
	g          graph.Directed
	byFullname map[string]*node
	built      map[string]error

	statusMu   sync.Mutex
	status     []string
	lastStatus time.Time
}

var isTerminal = func() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), unix.TCGETS)
	return err == nil
}()

func (s *scheduler) refreshStatus() {
	if !isTerminal {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.lastStatus = time.Now()
	var maxLen int
	for _, line := range s.status {
		if len(line) > maxLen {
			maxLen = len(line)
		}
	}
	for _, line := range s.status {
		if len(line) < maxLen {
			// overwrite stale characters with whitespace,
			// in every line to clear artifacts
			line += strings.Repeat(" ", maxLen-len(line))
		}
		fmt.Println(line)
	}
	fmt.Printf("\033[%dA", len(s.status)) // restore cursor position
}

func (s *scheduler) updateStatus(idx int, newStatus string) {
	if !isTerminal {
		return
	}
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if diff := len(s.status[idx]) - len(newStatus); diff > 0 {
		newStatus += strings.Repeat(" ", diff) // overwrite stale characters with whitespace
	}
	s.status[idx] = newStatus
	if time.Since(s.lastStatus) < 100*time.Millisecond {
		// printing status too frequently slows down the program
		return
	}
	s.lastStatus = time.Now()
	for _, line := range s.status {
		fmt.Println(line)
	}
	fmt.Printf("\033[%dA", len(s.status)) // restore cursor position
}

func (s *scheduler) buildDry(ctx context.Context, pkg string) bool {
	dur := 10*time.Millisecond + time.Duration(rand.Int63n(int64(1000*time.Millisecond)))
	//s.log.Printf("build of %s is taking %v", pkg, dur)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(dur):
	}
	return pkg != "libx11"
}

func (s *scheduler) build(ctx context.Context, pkg string) error {
	logFile, err := os.Create(filepath.Join(s.logDir, pkg+".log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	build := exec.CommandContext(ctx, "distri", "build")
	build.Dir = filepath.Join(s.distriRoot, "pkgs", pkg)
	build.Stdout = logFile
	build.Stderr = logFile
	if err := build.Run(); err != nil {
		return xerrors.Errorf("%v: %v", build.Args, err)
	}
	return nil
}

func (s *scheduler) run(ctx context.Context) error {
	numNodes := s.g.Nodes().Len()
	work := make(chan *node, numNodes)
	done := make(chan buildResult)
	eg, ctx := errgroup.WithContext(ctx)
	const freq = 1 * time.Second
	go func() {
		if err := trace.CPUEvents(ctx, freq); err != nil {
			s.log.Println(err)
			s.refreshStatus()
		}
	}()
	go func() {
		if err := trace.MemEvents(ctx, freq); err != nil {
			s.log.Println(err)
			s.refreshStatus()
		}
	}()

	for i := 0; i < s.workers; i++ {
		i := i // copy
		eg.Go(func() error {
			ticker := time.NewTicker(100 * time.Millisecond) // TODO: 1*time.Second
			defer ticker.Stop()
			for n := range work {
				if err := ctx.Err(); err != nil {
					return err
				}
				// Kick off the build
				{
					ev := trace.Event("build "+n.pkg, i)
					ev.Type = "B" // begin
					ev.Done()
				}
				s.updateStatus(i+1, "building "+n.pkg)
				start := time.Now()
				result := make(chan error)
				if s.simulate {
					go func() {
						if !s.buildDry(ctx, n.pkg) {
							result <- xerrors.Errorf("simulate intentionally failed")
						} else {
							result <- nil
						}
					}()
				} else {
					go func() {
						err := s.build(ctx, n.pkg)
						result <- err
					}()
				}

				// Wait for the build to complete while updating status
				var err error
			Build:
				for {
					select {
					case err = <-result:
						break Build
					case <-ticker.C:
						s.updateStatus(i+1, fmt.Sprintf("building %s since %v", n.pkg, time.Since(start)))
					}
				}

				select {
				case done <- buildResult{node: n, err: err}:
				case <-ctx.Done():
					return ctx.Err()
				}
				{
					ev := trace.Event("build "+n.pkg, i)
					ev.Type = "E" // end
					ev.Done()
				}
				s.updateStatus(i+1, "idle")
			}
			return nil
		})
	}

	// Enqueue all packages which have no dependencies to get the build started:
	for nodes := s.g.Nodes(); nodes.Next(); {
		n := nodes.Node()
		if s.g.From(n.ID()).Len() == 0 {
			select {
			case work <- n.(*node):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	go func() {
		defer close(work)
		succeeded := 0
		failed := 0
		for len(s.built) < numNodes { // scheduler tick
			select {
			case result := <-done:
				//s.log.Printf("build %s completed", result.name)
				n := s.byFullname[result.node.fullname]
				s.built[result.node.fullname] = result.err
				s.updateStatus(0, fmt.Sprintf("%d of %d packages: %d built, %d failed", len(s.built), numNodes, succeeded, failed))

				if result.err == nil {
					succeeded++
					for to := s.g.To(n.ID()); to.Next(); {
						if candidate := to.Node(); s.canBuild(candidate) {
							//s.log.Printf("  → enqueuing %s", candidate.(*node).name)
							work <- candidate.(*node)
						}
					}
				} else {
					s.log.Printf("build of %s failed (%v), see %s", result.node.pkg, result.err, filepath.Join(s.logDir, result.node.pkg+".log"))
					s.refreshStatus()
					failed += 1 + s.markFailed(n)
				}

			case <-ctx.Done():
				return
			}
		}
	}()
	if err := eg.Wait(); err != nil {
		return err
	}
	succeeded := 0
	for _, result := range s.built {
		if result == nil {
			succeeded++
		}
	}

	s.log.Printf("%d packages succeeded, %d failed, %d total", succeeded, len(s.built)-succeeded, len(s.built))

	return nil
}

func (s *scheduler) markFailed(n graph.Node) int {
	failed := 0
	//s.log.Printf("marking deps of %s as failed", n.(*node).name)
	for to := s.g.To(n.ID()); to.Next(); {
		d := to.Node()
		name := d.(*node).fullname
		//s.log.Printf("→ %s failed", name)
		if err, ok := s.built[name]; ok && err == nil {
			s.log.Fatalf("BUG: %s already succeeded, but dependencies cannot be fulfilled", name)
		}
		if _, ok := s.built[name]; !ok {
			s.built[d.(*node).fullname] = xerrors.Errorf("dependencies cannot be fulfilled")
			failed++
		}
		failed += s.markFailed(d)
	}
	return failed
}

// canBuild returns whether all dependencies of candidate are built.
func (s *scheduler) canBuild(candidate graph.Node) bool {
	//s.log.Printf("  checking %s", candidate.(*node).name)
	for from := s.g.From(candidate.ID()); from.Next(); {
		name := from.Node().(*node).fullname
		if err, ok := s.built[name]; !ok || err != nil {
			//s.log.Printf("  dep %s not yet ready", name)
			return false
		}
	}
	return true

}

// bison needs help2man <stage1>
// help2man needs perl
// perl needs glibc
// glibc needs bison
