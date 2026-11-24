package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
)

var (
	// ErrUnknownEpisode is returned for an episode ID that is not in the
	// database for the feed.
	ErrUnknownEpisode = errors.New("unknown episode")
	// ErrEpisodeNotFailed is returned by RetryEpisode for an episode whose
	// latest download attempt did not fail.
	ErrEpisodeNotFailed = errors.New("episode download has not failed")
)

// FailedEpisode describes an episode whose download failed, for the admin API.
// It carries counters, timestamps and the youtube-dl version, never raw error
// text. Title is provider metadata and must be treated as untrusted.
type FailedEpisode struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	PubDate       time.Time `json:"pub_date,omitzero"`
	Retries       int       `json:"retries"`
	FirstFailedAt time.Time `json:"first_failed_at,omitzero"`
	LastFailedAt  time.Time `json:"last_failed_at,omitzero"`
	FailedVersion string    `json:"failed_version,omitempty"`
	// State is "due" (attempted in the next update), "waiting" (see
	// NextAttemptAt) or "given_up" (until youtube-dl changes).
	State         string    `json:"state"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

// FailedEpisodeList is the answer of FailedEpisodes.
type FailedEpisodeList struct {
	FeedID string `json:"feed_id"`
	// Version is the installed youtube-dl version, which an episode's
	// FailedVersion is compared with.
	Version  string          `json:"youtube_dl_version,omitempty"`
	Episodes []FailedEpisode `json:"episodes"`
}

// FailedEpisodes lists the episodes of a feed whose latest download attempt
// failed, newest first, with where each stands in the retry policy. It
// returns ErrUnknownFeed for a feed that is not configured. It is safe to call
// from any goroutine.
func (s *Scheduler) FailedEpisodes(ctx context.Context, feedID string) (FailedEpisodeList, error) {
	if _, ok := s.currentFeeds()[feedID]; !ok {
		return FailedEpisodeList{}, ErrUnknownFeed
	}

	var (
		now     = s.now()
		version = s.downloader.Version()
		list    = []FailedEpisode{}
	)

	err := s.db.WalkEpisodes(ctx, feedID, func(episode *model.Episode) error {
		if episode.Status != model.EpisodeError {
			return nil
		}

		state, next := retryPlan(episode, now, version)
		list = append(list, FailedEpisode{
			ID:            episode.ID,
			Title:         episode.Title,
			PubDate:       episode.PubDate,
			Retries:       episode.Retries,
			FirstFailedAt: episode.FirstFailedAt,
			LastFailedAt:  episode.LastFailedAt,
			FailedVersion: episode.FailedVersion,
			State:         string(state),
			NextAttemptAt: next,
		})
		return nil
	})
	if err != nil {
		return FailedEpisodeList{}, err
	}

	sort.SliceStable(list, func(i, j int) bool {
		return list[i].PubDate.After(list[j].PubDate)
	})
	return FailedEpisodeList{FeedID: feedID, Version: version, Episodes: list}, nil
}

// RetryEpisode forgets the failed attempts of one episode, so the next update
// tries it again however long it has been failing, and then asks for a manual
// sync of its feed exactly like SyncNow.
//
// It returns ErrUnknownFeed, ErrUnknownEpisode or ErrEpisodeNotFailed without
// changing anything. If the reset succeeded but the sync was refused (rate
// limit or spacing), the error is the *RefusedError from SyncNow: the episode
// is then tried by the next update that runs. It is safe to call from any
// goroutine.
func (s *Scheduler) RetryEpisode(feedID, episodeID string) (FeedStatus, error) {
	if _, ok := s.currentFeeds()[feedID]; !ok {
		return FeedStatus{}, ErrUnknownFeed
	}

	err := s.db.UpdateEpisode(feedID, episodeID, func(episode *model.Episode) error {
		if episode.Status != model.EpisodeError {
			return ErrEpisodeNotFailed
		}
		episode.Status = model.EpisodeNew
		clearFailures(episode)
		return nil
	})
	switch {
	case errors.Is(err, model.ErrNotFound):
		return FeedStatus{}, ErrUnknownEpisode
	case err != nil:
		return FeedStatus{}, err
	}

	return s.SyncNow(feedID)
}

// DeleteEpisode removes an episode's downloaded media file (if present), marks
// the episode cleaned in BadgerDB so it is neither served in the RSS feed nor
// re-downloaded on future updates, and rebuilds the feed XML.
//
// It returns ErrUnknownFeed or ErrUnknownEpisode when the feed or episode is
// not found. Deleting an already-cleaned episode is idempotent. It is safe to
// call from any goroutine.
func (s *Scheduler) DeleteEpisode(ctx context.Context, feedID, episodeID string) error {
	feedConfig, ok := s.currentFeeds()[feedID]
	if !ok {
		return ErrUnknownFeed
	}

	if episodeID == "" || episodeID == "." || episodeID == ".." || strings.ContainsAny(episodeID, `/\`) {
		return ErrUnknownEpisode
	}

	ep, err := s.db.GetEpisode(ctx, feedID, episodeID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return ErrUnknownEpisode
	case err != nil:
		return err
	}

	if ep.Status != model.EpisodeCleaned {
		episodeName := feed.EpisodeName(feedConfig, ep)
		path := fmt.Sprintf("%s/%s", feedID, episodeName)

		// Mark cleaned in BadgerDB first so any in-flight download that finishes
		// concurrently observes EpisodeCleaned and removes its newly written file.
		err = s.db.UpdateEpisode(feedID, episodeID, func(episode *model.Episode) error {
			episode.Status = model.EpisodeCleaned
			episode.Title = ""
			episode.Description = ""
			clearFailures(episode)
			return nil
		})
		if err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return ErrUnknownEpisode
			}
			return err
		}

		if err := s.fs.Delete(ctx, path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete episode file %q: %w", path, err)
		}
	}

	if err := s.currentManager().buildXML(ctx, feedConfig); err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}

	return nil
}
