package main

import (
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/distr1/distri/internal/checkupstream"
	"github.com/distr1/distri/internal/env"
	"github.com/google/renameio"
	"github.com/protocolbuffers/txtpbfmt/parser"
)

type checker struct {
	updateVersion func(pkg, version string) error
}

func (c *checker) check1(pkg string) error {
	b, err := ioutil.ReadFile(filepath.Join(env.DistriRoot.PkgDir(pkg), "build.textproto"))
	if err != nil {
		return err
	}
	nodes, err := parser.Parse(b)
	if err != nil {
		return err
	}

	remote, err := checkupstream.Check(nodes)
	if err != nil {
		return err
	}
	if err := c.updateVersion(pkg, remote.Version); err != nil {
		return err
	}
	return nil
}

func logic(outputPath string) error {
	b, err := ioutil.ReadFile(outputPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	type upstreamStatus struct {
		UpstreamVersion string    `json:"upstream_version"`
		LastReachable   time.Time `json:"last_reachable"`
		Unreachable     bool      `json:"unreachable"`
	}
	var (
		packagesMu sync.Mutex
		packages   = make(map[string]upstreamStatus)
	)
	if len(b) > 0 {
		if err := json.Unmarshal(b, &packages); err != nil {
			return err
		}
	}
	// Mark every package as unreachable
	for pkg, cur := range packages {
		cur.Unreachable = true
		packages[pkg] = cur
	}
	c := &checker{
		updateVersion: func(pkg, version string) error {
			packagesMu.Lock()
			defer packagesMu.Unlock()
			cur := packages[pkg]
			cur.UpstreamVersion = version
			cur.LastReachable = time.Now()
			cur.Unreachable = false
			packages[pkg] = cur
			return nil
		},
	}
	fis, err := ioutil.ReadDir(env.DistriRoot.PkgDir(""))
	if err != nil {
		return err
	}
	workers := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for _, fi := range fis {
		pkg := fi.Name()
		wg.Add(1)
		go func() {
			defer wg.Done()
			workers <- struct{}{}
			defer func() { <-workers }()
			log.Printf("package %s", pkg)
			if err := c.check1(pkg); err != nil {
				log.Printf("check(%v): %v", pkg, err)
			}
		}()
	}
	wg.Wait()
	b, err = json.Marshal(&packages)
	if err != nil {
		return err
	}
	return renameio.WriteFile(outputPath, b, 0644)
}

func main() {
	var (
		outputPath = flag.String("output_path",
			"upstream_status.json",
			"path containing the current upstream status, if any, to which the output will be written")
	)
	flag.Parse()
	if err := logic(*outputPath); err != nil {
		log.Fatal(err)
	}
}
