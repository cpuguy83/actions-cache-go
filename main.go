package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/creachadair/gocache"
	"github.com/creachadair/gocache/cachedir"
	"github.com/pkg/errors"
	actionscache "github.com/tonistiigi/go-actions-cache"
	"golang.org/x/sync/singleflight"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	slog.SetDefault(slog.New(NewGitHubActionsHandler(slog.LevelInfo, os.Stderr)))

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

const (
	actionsResultURL = "ACTIONS_RESULTS_URL"
	actionsCacheURL  = "ACTIONS_CACHE_URL"
	actionsCacheV2   = "ACTIONS_CACHE_SERVICE_V2"
	actionsToken     = "ACTIONS_RUNTIME_TOKEN"
)

func do(ctx context.Context, cacheDirPath string, in io.Reader, out io.Writer) error {
	var (
		isV2 bool
		url  string
	)

	// https://github.com/actions/toolkit/blob/2b08dc18f261b9fdd978b70279b85cbef81af8bc/packages/cache/src/internal/config.ts#L19
	if v, ok := os.LookupEnv(actionsCacheV2); ok {
		if b, err := strconv.ParseBool(v); err == nil && b {
			isV2 = true
		}
	}

	if isV2 {
		if v, ok := os.LookupEnv(actionsResultURL); ok {
			url = v
		}
	} else {
		if v, ok := os.LookupEnv(actionsCacheURL); ok {
			url = v
		} else if v, ok := os.LookupEnv(actionsResultURL); ok {
			url = v
		}
	}

	if url == "" {
		return fmt.Errorf("missing %q or %q environment variable", actionsCacheURL, actionsResultURL)
	}

	client, err := actionscache.New(os.Getenv(actionsToken), url, isV2, actionscache.Opt{})
	if err != nil {
		return fmt.Errorf("error creating cache client: %w", err)
	}

	cacheDir, err := cachedir.New(cacheDirPath)
	if err != nil {
		return fmt.Errorf("error creating cache directory: %w", err)
	}

	var wg sync.WaitGroup

	var flight singleflight.Group
	srv := &gocache.Server{
		Get: handleGet(client, cacheDir),
		Put: handlePut(client, cacheDir, &wg, &flight),
		Close: func(context.Context) error {
			wg.Wait()
			return nil
		},
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
			return "", "", fmt.Errorf("error loading cache key %q: %w", actionID, err)
		}
		if entry == nil {
			return "", "", nil
		}

		remote := entry.Download(ctx)
		defer remote.Close()

		obj := gocache.Object{
			ActionID: actionID,
			Body:     io.NewSectionReader(remote, 0, math.MaxInt64),
			OutputID: actionID,
		}
		p, err := local.Put(ctx, obj)
		if err != nil {
			return "", "", fmt.Errorf("error storing in local cache: %w", err)
		}
		return "", p, nil
	}
}

func handlePut(client *actionscache.Cache, local *cachedir.Dir, wg *sync.WaitGroup, flight *singleflight.Group) putHandlerFunc {
	return func(ctx context.Context, req gocache.Object) (diskPath string, _ error) {
		p, err := local.Put(ctx, gocache.Object{
			ActionID: req.ActionID,
			Body:     req.Body,
			Size:     req.Size,
			OutputID: req.OutputID,
		})
		if err != nil {
			return "", fmt.Errorf("error storing in local cache: %w", err)
		}

		f, err := os.Open(p)
		if err != nil {
			return "", fmt.Errorf("error opening local cache file: %w", err)
		}
		wg.Add(1)

		go func() {
			defer f.Close()

			flight.Do(req.ActionID, func() (interface{}, error) {
				defer wg.Done()

				blob := &sectionReaderCloser{io.NewSectionReader(f, 0, req.Size), f}
				if err := client.Save(ctx, req.ActionID, blob); err != nil {
					var he actionscache.HTTPError

					var attrs []slog.Attr
					attrs = append(attrs, slog.String("actionID", req.ActionID))
					attrs = append(attrs, slog.String("message", err.Error()))
					if errors.As(err, &he) {
						if he.StatusCode == http.StatusConflict {
							// Cache already exists
							return nil, nil
						}
						attrs = append(attrs, slog.Int("statusCode", he.StatusCode))
					}
					slog.LogAttrs(ctx, slog.LevelError, "error saving to github actions cache", attrs...)
				}
				return nil, nil
			})
		}()

		return p, nil
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
