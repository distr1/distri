package main

import (
	"bufio"
	"context"
	"flag"
	"hash/fnv"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/distr1/distri/internal/addrfd"
	"github.com/distr1/distri/internal/env"
	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	bpb "github.com/distr1/distri/pb/builder"
)

const builderHelp = `distri builder [-flags]

builder runs a remote build server. This is useful to leverage additional
compute capacity, e.g. from a cluster or the public cloud.
`

type buildsrv struct {
	uploadBaseDir string
}

func (b *buildsrv) Store(srv bpb.Build_StoreServer) error {
	chunk, err := srv.Recv()
	if err != nil {
		return err
	}
	path := filepath.Join(b.uploadBaseDir, chunk.GetPath())
	if !strings.HasPrefix(path, filepath.Clean(b.uploadBaseDir)+"/") {
		return status.Errorf(codes.InvalidArgument, "path traversal detected")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if os.IsExist(err) {
			return status.Errorf(codes.AlreadyExists, "%v", err)
		}
		return err
	}
	defer f.Close()
	for {
		if _, err := f.Write(chunk.GetChunk()); err != nil {
			return err
		}

		chunk, err = srv.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return srv.SendAndClose(&bpb.StoreResponse{})
}

func (b *buildsrv) Build(req *bpb.BuildRequest, srv bpb.Build_BuildServer) error {
	// TODO: enforce minimum request deadline before starting a build

	for _, p := range req.GetInputPath() {
		path := filepath.Join(b.uploadBaseDir, p)
		if !strings.HasPrefix(path, filepath.Clean(b.uploadBaseDir)+"/") {
			return status.Errorf(codes.InvalidArgument, "path traversal detected")
		}

		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return status.Errorf(codes.NotFound, "%v", err)
			}
			return err
		}
		defer f.Close()
		h := fnv.New64()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		log.Printf("sum(%s) = %v", path, h.Sum64())
	}
	// TODO: use the hash of hashes as cache key, store <hash>→<artifacts> in a file

	// TODO: enforce inputs can only be read

	for _, subdir := range []string{"pkg", "debug"} {
		if err := os.MkdirAll(filepath.Join(b.uploadBaseDir, "build", "distri", subdir), 0755); err != nil {
			return err
		}
	}

	build := exec.CommandContext(srv.Context(),
		"distri",
		"build",
		"--artifactfd=3") // Go dup2()s ExtraFiles to 3 and onwards
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	build.ExtraFiles = []*os.File{w}
	build.Dir = filepath.Join(b.uploadBaseDir, req.GetWorkingDirectory())
	if !strings.HasPrefix(build.Dir, filepath.Clean(b.uploadBaseDir)+"/") {
		return status.Errorf(codes.InvalidArgument, "path traversal detected")
	}
	build.Env = []string{
		"DISTRIROOT=" + b.uploadBaseDir,
		"PATH=" + os.Getenv("PATH"), // for unshare
	}
	// TODO: write to durable storage, send as artifact in BuildProgress
	build.Stderr = os.Stderr
	build.Stdout = os.Stdout
	if err := build.Start(); err != nil {
		return err
	}
	// Close the write end of the pipe in the parent process.
	if err := w.Close(); err != nil {
		return err
	}
	var eg errgroup.Group
	eg.Go(func() error {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			if err := srv.Send(&bpb.BuildProgress{
				OutputPath: strings.Split(scanner.Text(), "\x00"),
			}); err != nil {
				return err
			}
		}
		return scanner.Err()
	})
	eg.Go(build.Wait)
	// TODO: send keepalive build progress messages
	if err := eg.Wait(); err != nil {
		return err
	}
	return nil
}

func (b *buildsrv) Retrieve(req *bpb.RetrieveRequest, srv bpb.Build_RetrieveServer) error {
	fn := filepath.Join(b.uploadBaseDir, req.GetPath())
	if !strings.HasPrefix(fn, filepath.Clean(b.uploadBaseDir)+"/") {
		return status.Errorf(codes.InvalidArgument, "path traversal detected")
	}

	f, err := os.Open(fn)
	if err != nil {
		if os.IsNotExist(err) {
			return status.Errorf(codes.NotFound, "%v", err)
		}
		return err
	}
	defer f.Close()
	const chunkSize = 1 * 1024 * 1024 // 1 MiB
	buf := make([]byte, chunkSize)
	path := req.GetPath()
	for {
		n, err := f.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if err := srv.Send(&bpb.Chunk{Path: path, Chunk: buf[:n]}); err != nil {
			return err
		}
		path = ""
	}
	return nil
}

func builder(ctx context.Context, args []string) error {
	fset := flag.NewFlagSet("builder", flag.ExitOnError)
	var (
		listenAddr = fset.String("listen",
			"localhost:2019",
			"[host]:port to serve gRPC requests on (unauthenticated)")

		uploadBaseDir = fset.String("upload_base_dir",
			"",
			"directory in which to store uploaded files")
	)
	addrfd := addrfd.RegisterFlags(fset)
	fset.Usage = usage(fset, builderHelp)
	fset.Parse(args)

	log.Printf("distriroot %q, listenAddr %q", env.DistriRoot, *listenAddr)

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		return err
	}
	addrfd.MustWrite(ln.Addr().String())
	srv := grpc.NewServer()
	bpb.RegisterBuildServer(srv, &buildsrv{
		uploadBaseDir: *uploadBaseDir,
	})
	reflection.Register(srv)
	return srv.Serve(ln)
}
