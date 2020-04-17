package repo

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/distr1/distri"
)

type ErrNotFound struct {
	url *url.URL
}

func (e ErrNotFound) Error() string {
	return fmt.Sprintf("%v: HTTP status 404", e.url)
}

type gzipReader struct {
	body io.ReadCloser
	zr   *gzip.Reader
}

func (r *gzipReader) Read(p []byte) (n int, err error) {
	return r.zr.Read(p)
}

func (r *gzipReader) Close() error {
	if err := r.zr.Close(); err != nil {
		return err
	}
	return r.body.Close()
}

type closeFuncReadCloser struct {
	reader    io.Reader
	closeFunc func() error
}

func (cfrc *closeFuncReadCloser) Read(p []byte) (n int, err error) {
	return cfrc.reader.Read(p)
}

func (cfrc *closeFuncReadCloser) Close() error {
	return cfrc.closeFunc()
}

var httpClient = &http.Client{Transport: &http.Transport{
	MaxIdleConnsPerHost: 10,
	DisableCompression:  true,
}}

func cacheFn(cache bool, repo distri.Repo, fn string) string {
	if !cache {
		return ""
	}
	ucd, err := os.UserCacheDir()
	if err != nil {
		log.Printf("cannot cache: %v", err)
		return ""
	}
	cacheFn := filepath.Join(ucd, "distri", strings.ReplaceAll(repo.PkgPath, "/", "_"), fn)
	if err := os.MkdirAll(filepath.Dir(cacheFn), 0755); err != nil {
		log.Printf("cannot cache: %v", err)
		return ""
	}
	return cacheFn
}

func Reader(ctx context.Context, repo distri.Repo, fn string, cache bool) (io.ReadCloser, error) {
	if !strings.HasPrefix(repo.PkgPath, "http://") &&
		!strings.HasPrefix(repo.PkgPath, "https://") {
		return os.Open(filepath.Join(repo.PkgPath, fn))
	}

	var ifModifiedSince time.Time
	cacheFn := cacheFn(cache, repo, fn)
	if cacheFn != "" {
		if st, err := os.Stat(cacheFn); err == nil {
			ifModifiedSince = st.ModTime()
		}
	}

	req, err := http.NewRequest("GET", repo.PkgPath+"/"+fn, nil) // TODO: sanitize slashes
	if err != nil {
		return nil, err
	}
	if !ifModifiedSince.IsZero() {
		req.Header.Set("If-Modified-Since", ifModifiedSince.Format(http.TimeFormat))
	}
	if os.Getenv("DISTRI_REEXEC") == "1" {
		req.Header.Set("X-Distri-Reexec", "yes")
	}
	// good for typical links (≤ gigabit)
	// performance bottleneck for faster links (10 gbit/s+)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	if cacheFn != "" && resp.StatusCode == http.StatusNotModified {
		return os.Open(cacheFn)
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		if got == http.StatusNotFound {
			return nil, &ErrNotFound{url: req.URL}
		}
		return nil, fmt.Errorf("%s: HTTP status %v", req.URL, resp.Status)
	}
	rdc := resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		rd, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		rdc = &gzipReader{body: resp.Body, zr: rd}
	}
	var cacheFile *os.File
	if cacheFn != "" {
		cacheFile, err = os.Create(cacheFn)
		if err != nil {
			log.Printf("cannot cache: %v", err)
		}
	}
	wr := ioutil.Discard
	if cacheFile != nil {
		wr = cacheFile
	}
	mtime := time.Now()
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		var err error
		mtime, err = time.Parse(http.TimeFormat, lm)
		if err != nil {
			log.Printf("invalid Last-Modified header %q", lm)
			mtime = time.Now()
		}
	}
	return &closeFuncReadCloser{
		reader: io.TeeReader(rdc, wr),
		closeFunc: func() error {
			if err := rdc.Close(); err != nil {
				return err
			}
			if cacheFile != nil {
				if err := cacheFile.Close(); err != nil {
					return err
				}
				if err := os.Chtimes(cacheFn, mtime, mtime); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}
