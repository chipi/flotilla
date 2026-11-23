package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/chipi/flotilla/internal/directory"
	"github.com/chipi/flotilla/internal/store"
)

// DirectoryConfig is where people come from (RFC-0014 §4.6): a source kind (file, the only one yet), its path, and how often it
// is read. The first sync runs at start.
type DirectoryConfig struct {
	Source string        `yaml:"source"`
	Path   string        `yaml:"path"`
	Every  time.Duration `yaml:"every"` // default 1h
}

// directoryCheckLines is what -check says about the directory: how many people the file names, and a warning for each pinned
// Slack user it does not name (such a user still approves within ADR-0005's limits, but "who approved" reads unknown).
// A file that cannot be read is an error: the deploy would start with a directory it cannot sync.
func directoryCheckLines(cfg *Config) ([]string, error) {
	if cfg.Directory == nil {
		return []string{"directory: none (every chat user, login and account is unknown)"}, nil
	}
	people, err := directory.ReadFile(cfg.Directory.Path)
	if err != nil {
		return nil, err
	}
	lines := []string{fmt.Sprintf("directory: %s %s, %d people, read every %s", cfg.Directory.Source, cfg.Directory.Path, len(people), cfg.Directory.Every)}
	if cfg.Slack != nil {
		pinned := make([]string, 0, len(cfg.Slack.Users))
		for id := range cfg.Slack.Users {
			pinned = append(pinned, id)
		}
		sort.Strings(pinned)
		for _, id := range directory.Missing(people, "slack", pinned) {
			lines = append(lines, fmt.Sprintf("WARN directory: the pinned Slack user %s (%s) is not in the people file", id, cfg.Slack.Users[id]))
		}
	}
	return lines, nil
}

// syncDirectory reads the source into the store's copy once. A source that cannot be read keeps the copy as it was: a
// directory outage never stops a case.
func syncDirectory(ctx context.Context, cfg *DirectoryConfig, st *store.Store, log *slog.Logger) {
	people, err := directory.ReadFile(cfg.Path)
	if err != nil {
		log.Error("directory: read failed, the copy stays as it was", "source", cfg.Source, "err", err)
		return
	}
	changes, err := st.SyncPeople(ctx, people, directory.Actor(cfg.Source))
	if err != nil {
		log.Error("directory: sync failed", "source", cfg.Source, "err", err)
		return
	}
	for _, c := range changes {
		log.Info("directory: "+c.Kind, "email", c.Email, "detail", c.Detail)
	}
}

// runDirectory syncs at start and then every cfg.Every until ctx ends.
func runDirectory(ctx context.Context, cfg *DirectoryConfig, st *store.Store, log *slog.Logger) {
	syncDirectory(ctx, cfg, st, log)
	t := time.NewTicker(cfg.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			syncDirectory(ctx, cfg, st, log)
		}
	}
}
