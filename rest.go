package main

import (
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	actionscache "github.com/tonistiigi/go-actions-cache"
)

const (
	apiURL           = "https://api.github.com"
	perPage          = 100
	defaultUserAgent = "go-actions-cache/1.0"
)

type RestAPI struct {
	repo  string
	token string
	opt   actionscache.Opt
}

func optsWithDefaults(opt actionscache.Opt) actionscache.Opt {
	if opt.Client == nil {
		opt.Client = http.DefaultClient
	}
	if opt.Timeout == 0 {
		opt.Timeout = 5 * time.Minute
	}
	if opt.BackoffPool == nil {
		opt.BackoffPool = &actionscache.BackoffPool{}
	}
	if opt.UserAgent == "" {
		opt.UserAgent = defaultUserAgent
	}
	return opt
}

func NewRestAPI(repo, token string, opt actionscache.Opt) (*RestAPI, error) {
	opt = optsWithDefaults(opt)
	return &RestAPI{
		repo:  repo,
		token: token,
		opt:   opt,
	}, nil
}

func (r *RestAPI) httpReq(ctx context.Context, method string, url *url.URL) (*http.Request, error) {
	req, err := http.NewRequest(method, url.String(), nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

func (r *RestAPI) ListKeys(ctx context.Context, prefix, ref string) iter.Seq2[[]actionscache.CacheKey, error] {
	page := 1

	return func(yield func([]actionscache.CacheKey, error) bool) {
		for {
			keys, total, err := r.listKeysPage(ctx, prefix, ref, page)
			if err != nil {
				if !yield(nil, err) {
					return
				}
			}

			slog.Debug("listKeysPage", "page", page, "total", total, "keys", len(keys), "prefix", prefix, "ref", ref)
			if !yield(keys, nil) {
				return
			}

			if total > page*perPage {
				page++
			} else {
				break
			}
		}
	}
}

func (r *RestAPI) listKeysPage(ctx context.Context, prefix, ref string, page int) ([]actionscache.CacheKey, int, error) {
	u, err := url.Parse(apiURL + "/repos/" + r.repo + "/actions/caches")
	if err != nil {
		return nil, 0, err
	}
	q := u.Query()
	q.Set("per_page", strconv.Itoa(perPage))
	if page > 0 {
		q.Set("page", strconv.Itoa(page))
	}
	if prefix != "" {
		q.Set("key", prefix)
	}
	if ref != "" {
		q.Set("ref", ref)
	}
	u.RawQuery = q.Encode()

	req, err := r.httpReq(ctx, "GET", u)
	if err != nil {
		return nil, 0, err
	}

	resp, err := r.opt.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}

	dec := json.NewDecoder(resp.Body)
	var keys struct {
		Total  int                     `json:"total_count"`
		Caches []actionscache.CacheKey `json:"actions_caches"`
	}

	if err := dec.Decode(&keys); err != nil {
		return nil, 0, err
	}

	resp.Body.Close()
	return keys.Caches, keys.Total, nil
}
