package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/model"
)

// failOnce runs one manual sync of feed A in which a1 fails and a2 downloads.
func failOnce(t *testing.T) *syncEnv {
	t.Helper()

	env := newSyncEnv(t, "A")
	env.dl.version = "2026.10.21"
	env.provider.set("A", syncEpisode("a1", 1), syncEpisode("a2", 2))
	env.dl.setFail("A/a1", errBreakage)
	env.run()

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)
	env.waitIdle()
	require.Equal(t, model.EpisodeError, env.episodes("A")["a1"].Status)
	return env
}

func TestFailedEpisodesReportsRetryState(t *testing.T) {
	env := failOnce(t)
	failedAt := env.clock.Now()

	list, err := env.s.FailedEpisodes(context.Background(), "A")
	require.NoError(t, err)

	assert.Equal(t, "A", list.FeedID)
	assert.Equal(t, "2026.10.21", list.Version)
	require.Len(t, list.Episodes, 1, "only the failed episode is listed")

	got := list.Episodes[0]
	assert.Equal(t, "a1", got.ID)
	assert.Equal(t, "Title a1", got.Title)
	assert.Equal(t, 1, got.Retries)
	assert.True(t, failedAt.Equal(got.FirstFailedAt))
	assert.True(t, failedAt.Equal(got.LastFailedAt))
	assert.Equal(t, "2026.10.21", got.FailedVersion)
	assert.Equal(t, string(retryWaiting), got.State)
	assert.True(t, failedAt.Add(time.Hour).Equal(got.NextAttemptAt))

	// After a self-update the same episode is due
	env.dl.mu.Lock()
	env.dl.version = "2026.11.23"
	env.dl.mu.Unlock()

	list, err = env.s.FailedEpisodes(context.Background(), "A")
	require.NoError(t, err)
	require.Len(t, list.Episodes, 1)
	assert.Equal(t, string(retryDue), list.Episodes[0].State)
	assert.True(t, list.Episodes[0].NextAttemptAt.IsZero())
}

func TestFailedEpisodesUnknownFeed(t *testing.T) {
	env := newSyncEnv(t, "A")

	_, err := env.s.FailedEpisodes(context.Background(), "NOPE")
	assert.ErrorIs(t, err, ErrUnknownFeed)
}

func TestFailedEpisodesEmptyListIsNotNil(t *testing.T) {
	env := newSyncEnv(t, "A")

	list, err := env.s.FailedEpisodes(context.Background(), "A")
	require.NoError(t, err)
	assert.NotNil(t, list.Episodes, "encodes as [] rather than null")
	assert.Empty(t, list.Episodes)
}

// RetryEpisode clears the failure record and queues a manual sync, which
// downloads the episode although its backoff has not run out.
func TestRetryEpisodeResetsAndSyncs(t *testing.T) {
	env := failOnce(t)
	env.dl.setFail("A/a1", nil)
	env.clock.Advance(ManualSyncSpacing + time.Second)

	status, err := env.s.RetryEpisode("A", "a1")
	require.NoError(t, err)
	assert.Equal(t, "A", status.ID)
	env.waitIdle()

	episode := env.episodes("A")["a1"]
	assert.Equal(t, model.EpisodeDownloaded, episode.Status)
	assert.Zero(t, episode.Retries)
	assert.Contains(t, env.xml("A"), "a1.mp3")

	list, err := env.s.FailedEpisodes(context.Background(), "A")
	require.NoError(t, err)
	assert.Empty(t, list.Episodes)
}

// When the follow-up sync is refused (here: too soon after the last manual
// run) the reset still stands, and the next update tries the episode.
func TestRetryEpisodeRefusedSyncStillResets(t *testing.T) {
	env := failOnce(t)
	env.dl.setFail("A/a1", nil)

	_, err := env.s.RetryEpisode("A", "a1")
	var refused *RefusedError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, ReasonTooSoon, refused.Reason)

	episode := env.episodes("A")["a1"]
	assert.Equal(t, model.EpisodeNew, episode.Status)
	assert.Zero(t, episode.Retries)
	assert.True(t, episode.LastFailedAt.IsZero())

	env.s.scheduleEnqueue("A")
	env.waitIdle()
	assert.Equal(t, model.EpisodeDownloaded, env.episodes("A")["a1"].Status)
}

func TestRetryEpisodeRejectsWhatItCannotReset(t *testing.T) {
	env := failOnce(t)
	before := env.episodes("A")

	_, err := env.s.RetryEpisode("NOPE", "a1")
	assert.ErrorIs(t, err, ErrUnknownFeed)

	_, err = env.s.RetryEpisode("A", "missing")
	assert.ErrorIs(t, err, ErrUnknownEpisode)

	_, err = env.s.RetryEpisode("A", "a2")
	assert.True(t, errors.Is(err, ErrEpisodeNotFailed), "got %v", err)

	assert.Equal(t, before, env.episodes("A"), "nothing changed")
	assert.False(t, env.s.Status().Busy, "nothing queued")
}

func TestDeleteEpisodePurgesFileAndRebuildsXML(t *testing.T) {
	env := failOnce(t)
	require.Equal(t, model.EpisodeDownloaded, env.episodes("A")["a2"].Status)
	require.Contains(t, env.xml("A"), "a2.mp3")

	err := env.s.DeleteEpisode(context.Background(), "A", "a2")
	require.NoError(t, err)

	ep := env.episodes("A")["a2"]
	assert.Equal(t, model.EpisodeCleaned, ep.Status)
	assert.Empty(t, ep.Title)
	assert.Empty(t, ep.Description)
	assert.NotContains(t, env.xml("A"), "a2.mp3")

	// Deleting again is idempotent.
	require.NoError(t, env.s.DeleteEpisode(context.Background(), "A", "a2"))

	// A subsequent sync must not re-download the purged episode.
	env.s.scheduleEnqueue("A")
	env.waitIdle()
	assert.Equal(t, model.EpisodeCleaned, env.episodes("A")["a2"].Status)
	assert.NotContains(t, env.xml("A"), "a2.mp3")
}

func TestDeleteEpisodeErrors(t *testing.T) {
	env := failOnce(t)

	assert.ErrorIs(t, env.s.DeleteEpisode(context.Background(), "NOPE", "a2"), ErrUnknownFeed)
	assert.ErrorIs(t, env.s.DeleteEpisode(context.Background(), "A", "missing"), ErrUnknownEpisode)
	assert.ErrorIs(t, env.s.DeleteEpisode(context.Background(), "A", "../a2"), ErrUnknownEpisode)
}

// Deleting an episode that has no media file on disk (EpisodeError / EpisodeNew)
// must succeed cleanly and mark it EpisodeCleaned instead of failing on ENOENT.
func TestDeleteEpisodeWithoutFileOnDisk(t *testing.T) {
	env := failOnce(t)
	require.Equal(t, model.EpisodeError, env.episodes("A")["a1"].Status)

	err := env.s.DeleteEpisode(context.Background(), "A", "a1")
	require.NoError(t, err)

	ep := env.episodes("A")["a1"]
	assert.Equal(t, model.EpisodeCleaned, ep.Status)
	assert.Empty(t, ep.Title)
	assert.Zero(t, ep.Retries)
	assert.NotContains(t, env.xml("A"), "a1.mp3")
}

// Deleting an episode while its download is in flight must not allow the
// finishing download to resurrect it as EpisodeDownloaded or leave an orphan file.
func TestDeleteEpisodeDuringInFlightDownload(t *testing.T) {
	env := newSyncEnv(t, "A")
	env.provider.set("A", syncEpisode("a1", 1))
	g := env.dl.addGate("A", "a1")
	env.run()

	_, err := env.s.SyncNow("A")
	require.NoError(t, err)

	// Wait until the download has started, then delete the episode mid-flight.
	<-g.started
	require.NoError(t, env.s.DeleteEpisode(context.Background(), "A", "a1"))
	close(g.release)

	env.waitIdle()

	ep := env.episodes("A")["a1"]
	assert.Equal(t, model.EpisodeCleaned, ep.Status)
	assert.Empty(t, ep.Title)
	assert.NotContains(t, env.xml("A"), "a1.mp3")
	_, statErr := os.Stat(filepath.Join(env.dataDir, "A", "a1.mp3"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
