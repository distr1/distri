package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"
	"github.com/golang/protobuf/proto"
	"golang.org/x/sync/errgroup"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

// batch builder.
// use cases:
// - continuous build system:
//   + a new commit comes in, mirror should be updated
//   + local modification to a local tree, rebuild all affected packages
//     → increment version numbers with a tool
//
// - build-all (e.g. create distri image)

// milestone: -bootstrap flag: print which packages need to be bootstrapped (those which depend on themselves)
//   needs cycle detection (e.g. pkg-config→glib→pkg-config)
// milestone: run a build action on a remote machine using cpu(1)

// build action
// - input: source tarball, build.textproto, build dep images
// - output: image

// to rebuild the archive: increment version number of all packages (helper tool which does this and commits?)

const batchHelp = `TODO

Packages which are already built (i.e. their .squashfs image exists) are skipped.

`

type node struct {
	id int64

	pkg      string // e.g. make
	fullname string // package and version, e.g. make-4.2.1
}

func (n *node) ID() int64 { return n.id }

func batch(args []string) error {
	fset := flag.NewFlagSet("batch", flag.ExitOnError)
	var (
		dryRun   = fset.Bool("dry_run", false, "only print packages which would otherwise be built")
		simulate = fset.Bool("simulate", false, "simulate builds by sleeping for random times instead of actually building packages")
		rebuild  = fset.Bool("rebuild", false, "rebuild all packages, regardless of whether they need to be built or not")
		jobs     = fset.Int("jobs", runtime.NumCPU(), "number of parallel jobs to run")
	)
	fset.Parse(args)

	log.Printf("distriroot %q", env.DistriRoot)

	// TODO: use simple.NewDirectedMatrix instead?
	g := simple.NewDirectedGraph()

	pkgsDir := filepath.Join(env.DistriRoot, "pkgs")
	fis, err := ioutil.ReadDir(pkgsDir)
	if err != nil {
		return err
	}
	byFullname := make(map[string]*node) // e.g. gcc-8.2.0
	byPkg := make(map[string]*node)      // e.g. gcc
	for idx, fi := range fis {
		pkg := fi.Name()

		// TODO(later): parallelize?
		buildTextprotoPath := filepath.Join(pkgsDir, fi.Name(), "build.textproto")
		c, err := ioutil.ReadFile(buildTextprotoPath)
		if err != nil {
			return err
		}
		var buildProto pb.Build
		if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
			return err
		}

		fullname := pkg + "-" + buildProto.GetVersion()
		if !*simulate {
			if squashStat, err := os.Stat(filepath.Join(env.DistriRoot, "build", "distri", "pkg", fullname+".squashfs")); err == nil {
				buildStat, err := os.Stat(buildTextprotoPath)
				if err != nil {
					return err
				}
				if !*rebuild && buildStat.ModTime().Before(squashStat.ModTime()) {
					continue // package already built
				}
				// fall-through: stale package
			}
		}

		// TODO: to conserve work, only add nodes which need to be rebuilt
		n := &node{
			id:       int64(idx),
			pkg:      pkg,
			fullname: fullname,
		}
		byPkg[n.pkg] = n
		byFullname[n.fullname] = n
		g.AddNode(n)
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
		deps = append(deps, builderdeps(&buildProto)...)
		deps = append(deps, buildProto.GetRuntimeDep()...)

		for _, dep := range deps {
			if dep == n.pkg+"-"+version ||
				dep == n.pkg {
				continue // TODO
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
				//log.Printf("  n %v", n)
				from := g.From(n.ID())
				for from.Next() {
					g.RemoveEdge(n.ID(), from.Node().ID())
				}
			}
		}
		if _, err := topo.Sort(g); err != nil {
			return fmt.Errorf("could not break cycles: %v", err)
		}
	}

	if *dryRun {
		log.Printf("build %d pkg", g.Nodes().Len())
		for it := g.Nodes(); it.Next(); {
			log.Printf("  build %s", it.Node().(*node).pkg)
		}
		return nil
	}

	logDir, err := ioutil.TempDir("", "distri-batch")
	if err != nil {
		return err
	}
	s := scheduler{
		logDir:     logDir,
		simulate:   *simulate,
		workers:    *jobs,
		g:          g,
		byFullname: byFullname,
		built:      make(map[string]error),
		status:     make([]string, *jobs+1),
	}
	if err := s.run(); err != nil {
		return err
	}

	return nil
}

type buildResult struct {
	node *node
	err  error
}

type scheduler struct {
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

func (s *scheduler) updateStatus(idx int, newStatus string) {
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

func (s *scheduler) buildDry(pkg string) bool {
	dur := 10*time.Millisecond + time.Duration(rand.Int63n(int64(1000*time.Millisecond)))
	//log.Printf("build of %s is taking %v", pkg, dur)
	time.Sleep(dur)
	return pkg != "libx11"
}

func (s *scheduler) build(pkg string) error {
	logFile, err := os.Create(filepath.Join(s.logDir, pkg+".log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	build := exec.Command("distri", "build")
	build.Dir = filepath.Join(env.DistriRoot, "pkgs", pkg)
	build.Stdout = logFile
	build.Stderr = logFile
	if err := build.Run(); err != nil {
		return fmt.Errorf("%v: %v", build.Args, err)
	}
	return nil
}

func (s *scheduler) run() error {
	numNodes := s.g.Nodes().Len()
	work := make(chan *node, numNodes)
	done := make(chan buildResult)
	eg, ctx := errgroup.WithContext(context.Background())
	for i := 0; i < s.workers; i++ {
		i := i // copy
		eg.Go(func() error {
			ticker := time.NewTicker(100 * time.Millisecond) // TODO: 1*time.Second
			defer ticker.Stop()
			for n := range work {
				// Kick off the build
				s.updateStatus(i+1, "building "+n.pkg)
				start := time.Now()
				result := make(chan error)
				if s.simulate {
					go func() {
						if !s.buildDry(n.pkg) {
							result <- fmt.Errorf("simulate intentionally failed")
						} else {
							result <- nil
						}
					}()
				} else {
					go func() {
						err := s.build(n.pkg)
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

				done <- buildResult{node: n, err: err}
				s.updateStatus(i+1, "idle")
			}
			return nil
		})
	}

	// Enqueue all packages which have no dependencies to get the build started:
	for nodes := s.g.Nodes(); nodes.Next(); {
		n := nodes.Node()
		if s.g.From(n.ID()).Len() == 0 {
			work <- n.(*node)
		}
	}
	go func() {
		defer close(work)
		succeeded := 0
		failed := 0
		for len(s.built) < numNodes { // scheduler tick
			select {
			case result := <-done:
				//log.Printf("build %s completed", result.name)
				n := s.byFullname[result.node.fullname]
				s.built[result.node.fullname] = result.err
				s.updateStatus(0, fmt.Sprintf("%d of %d packages: %d built, %d failed", len(s.built), numNodes, succeeded, failed))

				if result.err == nil {
					succeeded++
					for to := s.g.To(n.ID()); to.Next(); {
						if candidate := to.Node(); s.canBuild(candidate) {
							//log.Printf("  → enqueuing %s", candidate.(*node).name)
							work <- candidate.(*node)
						}
					}
				} else {
					log.Printf("build of %s failed (%v), see %s", result.node.pkg, result.err, filepath.Join(s.logDir, result.node.pkg+".log"))
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

	log.Printf("%d packages succeeded, %d failed, %d total", succeeded, len(s.built)-succeeded, len(s.built))

	return nil
}

func (s *scheduler) markFailed(n graph.Node) int {
	failed := 0
	//log.Printf("marking deps of %s as failed", n.(*node).name)
	for to := s.g.To(n.ID()); to.Next(); {
		d := to.Node()
		name := d.(*node).fullname
		//log.Printf("→ %s failed", name)
		if err, ok := s.built[name]; ok && err == nil {
			log.Fatalf("BUG: %s already succeeded, but dependencies cannot be fulfilled", name)
		}
		if _, ok := s.built[name]; !ok {
			s.built[d.(*node).fullname] = fmt.Errorf("dependencies cannot be fulfilled")
			failed++
		}
		failed += s.markFailed(d)
	}
	return failed
}

// canBuild returns whether all dependencies of candidate are built.
func (s *scheduler) canBuild(candidate graph.Node) bool {
	//log.Printf("  checking %s", candidate.(*node).name)
	for from := s.g.From(candidate.ID()); from.Next(); {
		name := from.Node().(*node).fullname
		if err, ok := s.built[name]; !ok || err != nil {
			//log.Printf("  dep %s not yet ready", name)
			return false
		}
	}
	return true

}

// bison needs help2man <stage1>
// help2man needs perl
// perl needs glibc
// glibc needs bison
