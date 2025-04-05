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

	level := slog.LevelInfo
	if v := os.Getenv("ACTIONS_CACHE_GO_DEBUG"); v != "" {
		strconv.ParseBool(v)
		level = slog.LevelDebug
		slog.SetLogLoggerLevel(level)
	}

	slog.SetDefault(slog.New(NewGitHubActionsHandler(level, os.Stderr)))

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
	actionsResultURL            = "ACTIONS_RESULTS_URL"
	actionsCacheURL             = "ACTIONS_CACHE_URL"
	actionsCacheV2              = "ACTIONS_CACHE_SERVICE_V2"
	actionsToken                = "ACTIONS_RUNTIME_TOKEN"
	actionsCacheGoPrefix        = "ACTIONS_CACHE_GO_PREFIX"
	restAPIToken                = "GITHUB_TOKEN"
	githubRepo                  = "GITHUB_REPOSITORY"
	defaultActionsCacheGoPrefix = "actions-cache-go-"
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

	prefix := defaultActionsCacheGoPrefix
	if v, ok := os.LookupEnv(actionsCacheGoPrefix); ok {
		prefix = v
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

	var restAPI *RestAPI
	token := os.Getenv(restAPIToken)
	repo := os.Getenv(githubRepo)
	if token != "" && repo != "" {
		slog.Debug("creating rest api client", "repo", repo)
		restAPI, err = NewRestAPI(repo, os.Getenv(restAPIToken), actionscache.Opt{})
		if err != nil {
			return fmt.Errorf("error creating rest api client: %w", err)
		}
	} else {
		if token == "" {
			slog.Info("Missing GITHUB_TOKEN environment variable, skipping rest api client. Performance may be degraded.")
		}
		if repo == "" {
			slog.Info("missing GITHUB_REPOSITORY environment variable, skipping rest api client. Performance may be degraded.")
		}
	}

	handler := &handler{
		client:  client,
		restAPI: restAPI,
		local:   cacheDir,
		prefix:  prefix,
	}

	srv := &gocache.Server{
		Get:   handler.handleGet,
		Put:   handler.handlePut,
		Close: handler.Close,
	}

	defer srv.Close(ctx)
	return srv.Run(ctx, in, out)
}

type handler struct {
	client  *actionscache.Cache
	restAPI *RestAPI
	local   *cachedir.Dir
	prefix  string

	flightGet singleflight.Group
	flightPut singleflight.Group

	keysOnce sync.Once
	keys     map[string]struct{}

	wg sync.WaitGroup
}

// initKeys initializes the keys map with all keys from the remote cache.
// This is done only once and is cached for the lifetime of the handler.
// This makes it so we don't need to make a network call for every key check.
func (h *handler) initKeys(ctx context.Context) {
	h.keysOnce.Do(func() {
		if h.restAPI == nil {
			return
		}

		slog.Debug("Initializing cache keys")
		defer slog.Debug("Initialized cache keys")

		for keys, err := range h.restAPI.ListKeys(ctx, h.prefix, "") {
			if err != nil {
				slog.Error("error listing keys", "error", err)
				return
			}

			if h.keys == nil {
				h.keys = make(map[string]struct{}, len(keys))
			}

			for _, key := range keys {
				slog.Debug("found cache key", "key", key.Key)
				h.keys[key.Key] = struct{}{}
			}
		}
	})
}

func (h *handler) Close(ctx context.Context) error {
	h.wg.Wait()
	return nil
}

func (h *handler) exists(ctx context.Context, key string) (bool, error) {
	h.initKeys(ctx)

	_, ok := h.keys[key]
	return ok, nil
}

type getRet struct {
	outputID string
	diskPath string
}

func (h *handler) handleGet(ctx context.Context, actionID string) (outputID, diskPath string, _ error) {
	actionID = h.prefix + actionID

	exists, err := h.exists(ctx, actionID)
	if err != nil {
		return "", "", fmt.Errorf("error checking if cache key exists: %w", err)
	}
	if !exists {
		// Key does not exist in the remote cache
		// Check if we have this locally
		id, path, err := h.local.Get(ctx, actionID)
		if err != nil {
			return "", "", err
		}
		return id, path, nil
	}

	v, err, _ := h.flightGet.Do(actionID, func() (interface{}, error) {
		id, path, err := h.local.Get(ctx, actionID)
		if err != nil {
			return nil, err
		}
		if id != "" {
			return &getRet{id, path}, nil
		}

		entry, err := h.client.Load(ctx, actionID)
		if err != nil {
			return nil, fmt.Errorf("error loading cache key %q: %w", actionID, err)
		}
		if entry == nil {
			slog.Debug("cache key not found", "actionID", actionID)
			return nil, nil
		}

		slog.Debug("cache key found", "actionID", actionID)

		remote := entry.Download(ctx)
		defer remote.Close()

		obj := gocache.Object{
			ActionID: actionID,
			Body:     io.NewSectionReader(remote, 0, math.MaxInt64),
			OutputID: actionID,
		}
		p, err := h.local.Put(ctx, obj)
		if err != nil {
			return nil, fmt.Errorf("error storing in local cache: %w", err)
		}
		return &getRet{id, p}, nil
	})

	if err != nil || v == nil {
		return "", "", err
	}

	vv := v.(*getRet)
	return vv.outputID, vv.diskPath, nil
}

func (h *handler) handlePut(ctx context.Context, req gocache.Object) (diskPath string, _ error) {
	req.ActionID = h.prefix + req.ActionID

	p, err := h.local.Put(ctx, gocache.Object{
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

	exists, err := h.exists(ctx, req.ActionID)
	if err != nil {
		slog.Error("error checking if cache key exists", "error", err)
	}
	if exists {
		// Don't need to upload if the cache already exists
		return p, nil
	}

	h.wg.Add(1)

	go func() {
		defer f.Close()

		h.flightPut.Do(req.ActionID, func() (interface{}, error) {
			defer h.wg.Done()

			blob := &sectionReaderCloser{io.NewSectionReader(f, 0, req.Size), f}
			if err := h.client.Save(ctx, req.ActionID, blob); err != nil {
				var he actionscache.HTTPError

				var attrs []slog.Attr
				attrs = append(attrs, slog.String("actionID", req.ActionID))
				if errors.As(err, &he) {
					if he.StatusCode == http.StatusConflict {
						// Cache already exists
						return nil, nil
					}
					attrs = append(attrs, slog.Int("statusCode", he.StatusCode))
				}
				attrs = append(attrs, slog.String("error", err.Error()))
				slog.LogAttrs(ctx, slog.LevelError, "error saving remote cache", attrs...)
			} else {
				slog.Debug("saved remote cache", "actionID", req.ActionID)
			}
			return nil, nil
		})
	}()

	return p, nil
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
