package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
	actionscache "github.com/tonistiigi/go-actions-cache"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cacheDirPath := filepath.Join(homeDir, ".cache", "actions-cache-go")
	if err := do(ctx, cacheDirPath, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func do(ctx context.Context, cacheDirPath string, in io.Reader, out io.Writer) error {
	client, err := actionscache.New(os.Getenv("ACTIONS_RUNTIME_TOKEN"), os.Getenv("ACTIONS_RUNTIME_URL"), true, actionscache.Opt{})
	if err != nil {
		return fmt.Errorf("error creating cache client: %w", err)
	}

	cacheDir, err := cachedir.New(cacheDirPath)
	if err != nil {
		return fmt.Errorf("error creating cache directory: %w", err)
	}

	srv := &gocache.Server{
		Get: handleGet(client, cacheDir),
		Put: handlePut(client, cacheDir),
	}

	defer srv.Close(ctx)
	return srv.Run(ctx, in, out)
}

type getHandlerFunc func(ctx context.Context, actionID string) (outputID, diskPath string, _ error)

type putHandlerFunc func(ctx context.Context, req gocache.Object) (diskPath string, _ error)

func handleGet(client *actionscache.Cache, local *cachedir.Dir) getHandlerFunc {
	return func(ctx context.Context, actionID string) (outputID, diskPath string, _ error) {
		id, path, err := local.Get(ctx, actionID)
		if err != nil {
			return "", "", err
		}
		if id != "" {
			return id, path, nil
		}

		entry, err := client.Load(ctx, actionID)
		if err != nil {
			var he *actionscache.HTTPError
			if errors.As(err, &he) && he.StatusCode == 404 {
				return "", "", nil
			}
			return "", "", err
		}

		remote := entry.Download(ctx)
		defer remote.Close()

		obj := gocache.Object{
			ActionID: actionID,
			Body:     io.NewSectionReader(remote, 0, math.MaxInt64),
		}
		p, err := local.Put(ctx, obj)
		if err != nil {
			return "", "", err
		}
		return "", p, nil
	}
}

func getActionBlob(r io.Reader, size int64) (actionscache.Blob, error) {
	if sr, ok := r.(*io.SectionReader); ok {
		if sr.Size() == size {
			return &sectionReaderCloser{sr, nil}, nil
		}
		return &sectionReaderCloser{io.NewSectionReader(sr, 0, size), nil}, nil
	}

	ra, ok := r.(io.ReaderAt)
	if ok {
		return &sectionReaderCloser{io.NewSectionReader(ra, 0, size), nil}, nil
	}

	tmp, err := os.CreateTemp("", "actions-cache-go-put-")
	if err != nil {
		return nil, err
	}
	return &sectionReaderCloser{io.NewSectionReader(tmp, 0, size), tmp}, nil
}

func handlePut(client *actionscache.Cache, local *cachedir.Dir) putHandlerFunc {
	return func(ctx context.Context, req gocache.Object) (diskPath string, _ error) {
		_, err := client.Load(ctx, req.ActionID) // check if it exists
		if err != nil {
			// Assume any error is a cache miss
			//
			blob, err := getActionBlob(req.Body, req.Size)
			if err != nil {
				return "", err
			}

			if err := client.Save(ctx, req.ActionID, blob); err != nil {
				return "", err
			}
		}

		return local.Put(ctx, gocache.Object{
			ActionID: req.ActionID,
			Body:     req.Body,
		})
	}
}

type sectionReaderCloser struct {
	*io.SectionReader
	io.Closer
}

func (c *sectionReaderCloser) Close() error {
	if c.Closer != nil {
		return c.Closer.Close()
	}
	return nil
}
