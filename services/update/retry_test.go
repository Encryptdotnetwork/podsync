package update

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/model"
)

const day = 24 * time.Hour

// errBreakage is what yt-dlp printed on 23 Nov 2026 while its n-challenge
// solver could not handle a new YouTube player: the same text a video without
// usable formats produces.
var errBreakage = errors.New("ERROR: [youtube] e1: Requested format is not available")

func TestRetryBackoff(t *testing.T) {
	for retries, want := range map[int]time.Duration{
		0:  0,
		1:  time.Hour,
		2:  2 * time.Hour,
		3:  4 * time.Hour,
		4:  8 * time.Hour,
		5:  16 * time.Hour,
		6:  24 * time.Hour,
		50: 24 * time.Hour,
	} {
		assert.Equal(t, want, retryBackoff(retries), "retries=%d", retries)
	}
}

func TestRetryPlan(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	// failed is an episode that failed retries times, the first one first ago
	// and the latest one last ago, the latest with youtube-dl version.
	failed := func(retries int, first, last time.Duration, version string) *model.Episode {
		return &model.Episode{
			ID:            "e1",
			Status:        model.EpisodeError,
			Retries:       retries,
			FirstFailedAt: now.Add(-first),
			LastFailedAt:  now.Add(-last),
			FailedVersion: version,
		}
	}

	tests := []struct {
		name    string
		episode *model.Episode
		version string
		state   retryState
		next    time.Time
	}{
		{"new episode", &model.Episode{Status: model.EpisodeNew}, "v1", retryDue, time.Time{}},
		{"failed before timestamps were recorded", &model.Episode{Status: model.EpisodeError, Retries: 5}, "v1", retryDue, time.Time{}},
		{"first failure 30 minutes ago", failed(1, 30*time.Minute, 30*time.Minute, "v1"), "v1", retryWaiting, now.Add(30 * time.Minute)},
		{"first failure 56 minutes ago, within the slack", failed(1, 56*time.Minute, 56*time.Minute, "v1"), "v1", retryDue, time.Time{}},
		{"third failure 3 hours ago waits 4 hours", failed(3, 4*time.Hour, 3*time.Hour, "v1"), "v1", retryWaiting, now.Add(time.Hour)},
		{"five failures over 13 days", failed(5, 13*day, 30*time.Minute, "v1"), "v1", retryWaiting, now.Add(15*time.Hour + 30*time.Minute)},
		{"five failures over 14 days", failed(5, 14*day+time.Hour, time.Hour, "v1"), "v1", retryGivenUp, time.Time{}},
		{"four failures over 20 days are not enough", failed(4, 20*day, 9*time.Hour, "v1"), "v1", retryDue, time.Time{}},
		{"given up, then youtube-dl changed", failed(20, 15*day, time.Hour, "v1"), "v2", retryDue, time.Time{}},
		{"waiting, then youtube-dl changed", failed(1, 10*time.Minute, 10*time.Minute, "v1"), "v2", retryDue, time.Time{}},
		{"version of the failure unknown", failed(1, 10*time.Minute, 10*time.Minute, ""), "v2", retryWaiting, now.Add(50 * time.Minute)},
		{"current version unknown", failed(1, 10*time.Minute, 10*time.Minute, "v1"), "", retryWaiting, now.Add(50 * time.Minute)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, next := retryPlan(tt.episode, now, tt.version)
			assert.Equal(t, tt.state, state)
			assert.True(t, tt.next.Equal(next), "next attempt: want %s, got %s", tt.next, next)
		})
	}
}

func TestRecordFailureAndClearFailures(t *testing.T) {
	start := time.Date(2026, 11, 23, 20, 35, 0, 0, time.UTC)
	episode := &model.Episode{ID: "e1", Status: model.EpisodeNew}

	recordFailure(episode, start, "v1")
	recordFailure(episode, start.Add(time.Hour), "v2")

	assert.Equal(t, model.EpisodeError, episode.Status)
	assert.Equal(t, 2, episode.Retries)
	assert.True(t, start.Equal(episode.FirstFailedAt))
	assert.True(t, start.Add(time.Hour).Equal(episode.LastFailedAt))
	assert.Equal(t, "v2", episode.FailedVersion)

	clearFailures(episode)
	assert.Zero(t, episode.Retries)
	assert.True(t, episode.FirstFailedAt.IsZero())
	assert.True(t, episode.LastFailedAt.IsZero())
	assert.Empty(t, episode.FailedVersion)
}

// Container restarts run an update straight away. They must not use up the
// attempts of a failing episode: the next attempt waits for the backoff, and a
// success clears the failure record.
func TestUpdateRestartsDoNotUseUpAttempts(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.downloader.version = "2026.10.21"
	env.downloader.errs["e1"] = errBreakage

	start := env.clock.Now()
	require.NoError(t, env.update(t))

	// Two restarts within the hour
	env.clock.Advance(10 * time.Minute)
	require.NoError(t, env.update(t))
	env.clock.Advance(20 * time.Minute)
	require.NoError(t, env.update(t))

	assert.Len(t, env.downloader.calls, 1)
	episode := env.episodes(t)["e1"]
	assert.Equal(t, model.EpisodeError, episode.Status)
	assert.Equal(t, 1, episode.Retries)
	assert.True(t, start.Equal(episode.FirstFailedAt))
	assert.True(t, start.Equal(episode.LastFailedAt))
	assert.Equal(t, "2026.10.21", episode.FailedVersion)

	// The hourly cycle after the failure tries again, and this time it works
	env.clock.Advance(30 * time.Minute)
	delete(env.downloader.errs, "e1")
	require.NoError(t, env.update(t))

	assert.Len(t, env.downloader.calls, 2)
	episode = env.episodes(t)["e1"]
	assert.Equal(t, model.EpisodeDownloaded, episode.Status)
	assert.Zero(t, episode.Retries)
	assert.True(t, episode.FirstFailedAt.IsZero())
	assert.True(t, episode.LastFailedAt.IsZero())
	assert.Empty(t, episode.FailedVersion)
	assert.FileExists(t, env.mediaPath("e1"))
	assert.Contains(t, env.xml(t), "e1.mp3")
}

// The 23 Nov 2026 breakage lasted more than a day. With hourly updates the
// episode is attempted at 0, 1, 3, 7, 15, 31 and 55 hours, is still not given
// up after three days, and the yt-dlp self-update that fixes the breakage gets
// it downloaded at once.
func TestUpdateSurvivesMultiDayBreakage(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.downloader.version = "2026.10.21"
	env.downloader.errs["e1"] = errBreakage

	for hour := 0; hour < 72; hour++ {
		require.NoError(t, env.update(t))
		env.clock.Advance(time.Hour)
	}

	assert.Len(t, env.downloader.calls, 7)
	episode := env.episodes(t)["e1"]
	assert.Equal(t, 7, episode.Retries)
	assert.False(t, givenUp(episode))

	// The next hourly update would still wait (24 hours from hour 55), but
	// the self-update to a fixed version makes the episode due right away
	state, _ := retryPlan(episode, env.clock.Now(), "2026.10.21")
	require.Equal(t, retryWaiting, state)

	env.downloader.version = "2026.11.23"
	delete(env.downloader.errs, "e1")
	require.NoError(t, env.update(t))

	assert.Len(t, env.downloader.calls, 8)
	assert.Equal(t, model.EpisodeDownloaded, env.episodes(t)["e1"].Status)
	assert.Contains(t, env.xml(t), "e1.mp3")
}

// A video that keeps failing (deleted, private or geo-blocked but still
// listed) is given up after two weeks of failures, then gets exactly one more
// attempt per new youtube-dl version.
func TestUpdateGivesUpAfterRetryWindow(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.downloader.version = "v1"
	env.downloader.errs["e1"] = errors.New("ERROR: [youtube] e1: Video unavailable")

	// Daily updates: every one of them is due, until the failures span 14 days
	for i := 0; i < 20; i++ {
		require.NoError(t, env.update(t))
		env.clock.Advance(day)
	}

	assert.Len(t, env.downloader.calls, 15, "attempts on days 0 to 14")
	episode := env.episodes(t)["e1"]
	assert.Equal(t, 15, episode.Retries)
	assert.True(t, givenUp(episode))
	assert.Equal(t, retryWindow, episode.LastFailedAt.Sub(episode.FirstFailedAt))

	// A new youtube-dl version: one attempt, then given up again
	env.downloader.version = "v2"
	require.NoError(t, env.update(t))
	env.clock.Advance(day)
	require.NoError(t, env.update(t))

	assert.Len(t, env.downloader.calls, 16)
	episode = env.episodes(t)["e1"]
	assert.Equal(t, "v2", episode.FailedVersion)
	assert.True(t, givenUp(episode))

	// Never in the feed, which is still built
	assert.NotContains(t, env.xml(t), "e1.mp3")
}

// Records written before the retry timestamps existed (Retries only) are
// retried at the next update, and their window starts with that attempt.
func TestUpdateRetriesEpisodeGivenUpByOlderRelease(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour))
	env.downloader.errs["e1"] = errBreakage
	require.NoError(t, env.update(t))

	// What the previous release stored for an episode it gave up on
	require.NoError(t, env.db.UpdateEpisode(testFeedID, "e1", func(episode *model.Episode) error {
		episode.Retries = maxDownloadAttempts
		episode.FirstFailedAt = time.Time{}
		episode.LastFailedAt = time.Time{}
		episode.FailedVersion = ""
		return nil
	}))

	env.clock.Advance(time.Minute)
	require.NoError(t, env.update(t))

	assert.Len(t, env.downloader.calls, 2)
	episode := env.episodes(t)["e1"]
	assert.Equal(t, maxDownloadAttempts+1, episode.Retries)
	assert.True(t, env.clock.Now().Equal(episode.FirstFailedAt))
	assert.False(t, givenUp(episode))

	// From here on it waits like any other failure (24 hours after 6 failures)
	env.clock.Advance(time.Hour)
	require.NoError(t, env.update(t))
	assert.Len(t, env.downloader.calls, 2)
}

// An episode waiting for its next attempt does not take one of the page_size
// download slots of the cycle.
func TestUpdateWaitingEpisodeLeavesDownloadSlot(t *testing.T) {
	env := newUpdaterEnv(t, testEpisode("e1", time.Hour), testEpisode("e2", 2*time.Hour))
	env.cfg.PageSize = 1
	env.downloader.errs["e1"] = errBreakage

	require.NoError(t, env.update(t))
	assert.Equal(t, []string{"e1"}, env.downloader.calls)

	env.clock.Advance(10 * time.Minute)
	require.NoError(t, env.update(t))
	assert.Equal(t, []string{"e1", "e2"}, env.downloader.calls)
	assert.Equal(t, model.EpisodeDownloaded, env.episodes(t)["e2"].Status)
}
