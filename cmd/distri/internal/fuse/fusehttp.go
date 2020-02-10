package fuse

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/renameio"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
)

// TODO: to make this performant enough, even just for starting emacs, we
// probably need to cache relatively large blocks.

var httpClient = &http.Client{Transport: &http.Transport{
	// http.DefaultMaxIdleConnsPerHost is 2, which is not enough for concurrent
	// requests.
	MaxIdleConnsPerHost: 1024,
}}

type httpReaderAt struct {
	fileurl string // e.g. http://localhost:7080/bash-4.4.18.squashfs
}

func (hr *httpReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	req, err := http.NewRequest("GET", hr.fileurl, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))))
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusPartialContent; got != want {
		return 0, xerrors.Errorf("HTTP status %v", resp.Status)
	}
	for n < int(resp.ContentLength-1) {
		nn, err := resp.Body.Read(p[n:])
		if err != nil {
			return 0, err
		}
		n += nn
	}

	io.Copy(ioutil.Discard, resp.Body) // ensure resp.Body hits EOF

	log.Printf("[%s] %d-%d (len %d), read %d (content-length %d)", filepath.Base(hr.fileurl), off, off+int64(len(p)), len(p), n, resp.ContentLength)
	return n, err
}

func autodownload(imgDir, remote, rel string) (*os.File, error) {
	fileurl := remote + "/" + rel
	dest := filepath.Join(imgDir, filepath.Base(fileurl))

	// If the file can be opened, it was successfully downloaded already. As
	// files never change (only new files are added), no update check is needed.
	if f, err := os.Open(dest); err == nil {
		return f, nil
	}

	var (
		eg       errgroup.Group
		base     = strings.TrimSuffix(dest, ".squashfs")
		baseurl  = strings.TrimSuffix(fileurl, ".squashfs")
		files    = make(map[string]*renameio.PendingFile)
		suffixes = []string{".squashfs"}
	)
	if !strings.HasPrefix(rel, "debug/") {
		suffixes = append(suffixes, ".meta.textproto")
	}
	for _, suffix := range suffixes {
		suffix := suffix // copy
		f, err := renameio.TempFile("", base+suffix)
		if err != nil {
			return nil, err
		}
		defer f.Cleanup()
		files[suffix] = f
		eg.Go(func() error {
			resp, err := http.Get(baseurl + suffix)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if got, want := resp.StatusCode, http.StatusOK; got != want {
				return xerrors.Errorf("%s: HTTP status %v", baseurl+suffix, resp.Status)
			}
			_, err = io.Copy(f, resp.Body)
			return err
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, err
	}
	for _, suffix := range suffixes {
		if err := files[suffix].CloseAtomicallyReplace(); err != nil {
			return nil, err
		}
	}

	return os.Open(dest)
}
