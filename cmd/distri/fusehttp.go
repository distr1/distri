package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
		return 0, fmt.Errorf("HTTP status %v", resp.Status)
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

func updateAndOpen(imgDir, fileurl string) (*os.File, error) {
	// TODO: use etags (?) or if-modified-since for skipping the download if the file is unchanged

	resp, err := http.Get(fileurl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		return nil, fmt.Errorf("HTTP status %v", resp.Status)
	}
	f, err := os.Create(filepath.Join(imgDir, filepath.Base(fileurl)))
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	return f, nil
}
