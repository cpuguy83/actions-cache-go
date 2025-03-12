package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"strings"
)

// GitHubActionsHandler is a custom slog handler that formats log output for GitHub Actions annotations.
type GitHubActionsHandler struct {
	level slog.Level
	out   io.Writer

	attrs map[string]slog.Value
}

// NewGitHubActionsHandler creates a new GitHubActionsHandler with the specified log level.
func NewGitHubActionsHandler(level slog.Level, outStream io.Writer) *GitHubActionsHandler {
	return &GitHubActionsHandler{level: level, out: outStream}
}

// Enabled reports whether the handler is enabled for the given level.
func (h *GitHubActionsHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle formats the log record as a GitHub Actions annotation and prints it.
func (h *GitHubActionsHandler) Handle(ctx context.Context, r slog.Record) error {
	var annotationType string
	switch r.Level {
	case slog.LevelError:
		annotationType = "error"
	case slog.LevelWarn:
		annotationType = "warning"
	default:
		annotationType = "notice"
	}

	var attrs map[string]slog.Value
	if len(h.attrs) > 0 {
		attrs = maps.Clone(h.attrs)
	}

	r.Attrs(func(attr slog.Attr) bool {
		if h.attrs == nil {
			attrs = make(map[string]slog.Value, r.NumAttrs())
		}

		attrs[attr.Key] = attr.Value
		return true
	})

	var attrSl []string
	if len(attrs) > 0 {
		attrSl = make([]string, 0, len(attrs))

		for k, v := range attrs {
			attrSl = append(attrSl, fmt.Sprintf("%s=%v", k, v))
		}
	}

	fmt.Fprintf(h.out, "::%s::%s %s\n",
		annotationType,
		r.Message,
		strings.Join(attrSl, ", "),
	)

	return nil
}

// WithAttrs returns a new handler with the given attributes.
func (h *GitHubActionsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := maps.Clone(h.attrs)
	if newAttrs == nil {
		newAttrs = make(map[string]slog.Value, len(attrs))
	}
	for _, attr := range attrs {
		newAttrs[attr.Key] = attr.Value
	}
	return &GitHubActionsHandler{
		level: h.level,
		out:   h.out,
		attrs: newAttrs,
	}
}

// WithGroup returns a new handler with the given group name.
func (h *GitHubActionsHandler) WithGroup(name string) slog.Handler {
	return h
}
