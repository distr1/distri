package main

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"

	"github.com/golang/protobuf/proto"
	"github.com/google/go-cmp/cmp"

	"github.com/distr1/distri/internal/build"
	"github.com/distr1/distri/internal/distritest"
	"github.com/distr1/distri/internal/env"
	"github.com/distr1/distri/pb"

	bpb "github.com/distr1/distri/pb/builder"
)

func TestBuilder(t *testing.T) {
	ctx, canc := context.WithCancel(context.Background())
	defer canc()
	tmp, err := ioutil.TempDir("", "distri-test-builder")
	if err != nil {
		t.Fatal(err)
	}
	defer distritest.RemoveAll(t, tmp)
	go func() {
		// TODO: -listen=localhost:0 and -addrfd = fd of a pipe or sth
		if err := builder(ctx, []string{"-upload_base_dir=" + tmp}); err != nil {
			t.Fatal(err)
		}
	}()

	conn, err := grpc.DialContext(ctx, "localhost:2019", grpc.WithInsecure(), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	cl := bpb.NewBuildClient(conn)

	const path = "subdir/src.tar.gz"
	var succeeded bool
	want := []byte("first-chunk\nsecond-chunk\n")
	t.Run("Upload", func(t *testing.T) {
		upcl, err := cl.Store(ctx)
		if err != nil {
			t.Fatal(err)
		}

		if err := upcl.Send(&bpb.Chunk{
			Path:  path,
			Chunk: []byte("first-chunk\n"),
		}); err != nil {
			t.Fatal(err)
		}
		if err := upcl.Send(&bpb.Chunk{
			Chunk: []byte("second-chunk\n"),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := upcl.CloseAndRecv(); err != nil {
			t.Fatal(err)
		}

		got, err := ioutil.ReadFile(filepath.Join(tmp, path))
		if err != nil {
			t.Fatal(err)
		}

		if diff := cmp.Diff(string(want), string(got)); diff != "" {
			t.Fatalf("uploaded file differs: diff (-want +got):\n%s", diff)
		}
		succeeded = true
	})
	if !succeeded {
		t.Skip("TestBuilder/Upload failed")
	}

	b, err := build.NewCtx()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ioutil.ReadFile(filepath.Join(env.DistriRoot, "pkgs", "hello", "build.textproto"))
	if err != nil {
		t.Fatal(err)
	}
	var buildProto pb.Build
	if err := proto.UnmarshalText(string(c), &buildProto); err != nil {
		t.Fatal(err)
	}
	deps := buildProto.GetDep()
	deps = append(deps, b.Builderdeps(&buildProto)...)
	deps = append(deps, buildProto.GetRuntimeDep()...)

	resolved, err := b.GlobAndResolve(env.DefaultRepo, deps, "")
	if err != nil {
		t.Fatal(err)
	}
	expanded := make([]string, 0, 2*len(resolved))
	for _, r := range resolved {
		expanded = append(expanded, r+".meta.textproto")
		expanded = append(expanded, r+".squashfs")
	}

	prefixed := make([]string, len(expanded))
	for i, e := range expanded {
		prefixed[i] = "build/distri/pkg/" + e
	}

	inputs := append([]string{
		"pkgs/hello/build.textproto",
	}, prefixed...)
	for _, input := range inputs {
		log.Printf("store(%s)", input)
		if err := store(ctx, cl, input); err != nil {
			t.Fatal(err)
		}
	}

	var artifacts []string
	t.Run("Build", func(t *testing.T) {
		bcl, err := cl.Build(ctx, &bpb.BuildRequest{
			WorkingDirectory: "pkgs/hello",
			InputPath:        inputs,
		})
		if err != nil {
			t.Fatal(err)
		}
		for {
			progress, err := bcl.Recv()
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			artifacts = append(artifacts, progress.GetOutputPath()...)
			log.Printf("progress: %+v", progress)
		}
	})

	var squashfs string
	for _, artifact := range artifacts {
		if strings.HasPrefix(artifact, "build/distri/pkg/hello") &&
			strings.HasSuffix(artifact, ".squashfs") {
			squashfs = artifact
			break
		}
	}
	if squashfs == "" {
		t.Fatalf("build: no .squashfs output found")
	}

	t.Run("Download/"+squashfs, func(t *testing.T) {
		downcl, err := cl.Retrieve(ctx, &bpb.RetrieveRequest{
			Path: squashfs,
		})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		for {
			chunk, err := downcl.Recv()
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			if _, err := buf.Write(chunk.GetChunk()); err != nil {
				t.Fatal(err)
			}
		}
		// TODO: open buf as a squashfs file
	})
}
