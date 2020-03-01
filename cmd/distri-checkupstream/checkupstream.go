package main

import (
	"database/sql"
	"flag"
	"io/ioutil"
	"log"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/distr1/distri/internal/checkupstream"
	"github.com/distr1/distri/internal/env"
	"github.com/protocolbuffers/txtpbfmt/parser"

	// PostgreSQL driver for database/sql:
	_ "github.com/lib/pq"
)

type checker struct {
	db            *sql.DB
	updateVersion *sql.Stmt
}

func (c *checker) check1(pkg string) error {
	b, err := ioutil.ReadFile(filepath.Join(env.DistriRoot, "pkgs", pkg, "build.textproto"))
	if err != nil {
		return err
	}
	nodes, err := parser.Parse(b)
	if err != nil {
		return err
	}

	_, _, version, err := checkupstream.Check(nodes)
	if err != nil {
		return err
	}
	if _, err := c.updateVersion.Exec(pkg, version); err != nil {
		return err
	}
	return nil
}

func logic() error {
	db, err := sql.Open("postgres", "dbname=distri sslmode=disable")
	if err != nil {
		return err
	}
	markUnreachable, err := db.Prepare(`UPDATE upstream_status SET unreachable = true`)
	if err != nil {
		return err
	}
	updateVersion, err := db.Prepare(`
INSERT INTO upstream_status (package, upstream_version, last_reachable, unreachable) VALUES ($1, $2, NOW(), false)
ON CONFLICT (package) DO UPDATE SET upstream_version = $2, last_reachable = NOW(), unreachable = false
`)
	c := &checker{
		db:            db,
		updateVersion: updateVersion,
	}
	fis, err := ioutil.ReadDir(filepath.Join(env.DistriRoot, "pkgs"))
	if err != nil {
		return err
	}
	// TODO: refactor to collect check1() results, run all db modifications in a
	// transaction
	if _, err := markUnreachable.Exec(); err != nil {
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
	return nil
}

func main() {
	flag.Parse()
	if err := logic(); err != nil {
		log.Fatal(err)
	}
}
