package update

import (
	"time"

	"github.com/mxpv/podsync/pkg/model"
)

// Retry policy for episodes whose download failed.
//
// youtube-dl reports a temporary breakage (a YouTube player change that the
// installed version cannot handle, say) with the same messages as a video that
// is really gone, so the policy does not read the error. Instead it spaces the
// attempts out in time and only gives up on failures that persist:
//
//   - After a failure the episode waits retryBackoff(Retries): 1h, 2h, 4h, 8h,
//     16h, then 24h between attempts. The wait is measured from the failure, so
//     extra update cycles (every container restart runs one) don't use up
//     attempts.
//   - It is given up once it has failed at least maxDownloadAttempts times over
//     at least retryWindow.
//   - An episode whose latest failure was made by a different youtube-dl version
//     is retried at once, waiting or given up: a self-update is the usual fix
//     for an extractor breakage. It gets one such attempt per version.
//
// Episodes that failed before this policy existed have Retries but no
// timestamps. They are retried at the next update, and their window starts
// with that attempt.

const (
	// maxDownloadAttempts is the least number of failed attempts before an
	// episode can be given up.
	maxDownloadAttempts = 5

	// retryWindow is how long failures must persist before an episode is given
	// up. It is measured from the first failure to the latest one.
	retryWindow = 14 * 24 * time.Hour

	// Delay between attempts: retryBackoffInitial after the first failure,
	// doubling up to retryBackoffMax.
	retryBackoffInitial = time.Hour
	retryBackoffMax     = 24 * time.Hour

	// retrySlack lets an attempt start a little before its due time. Update
	// cycles of a feed start at a fixed period while the failure is recorded a
	// little after its cycle started, so without it an hourly feed would retry
	// one cycle late every time.
	retrySlack = 5 * time.Minute
)

// retryState is where a failed episode stands in the retry policy.
type retryState string

const (
	retryDue     retryState = "due"      // attempt it in the next update
	retryWaiting retryState = "waiting"  // too soon after the latest failure
	retryGivenUp retryState = "given_up" // failures persisted, no more attempts
)

// retryPlan reports whether the episode is due for a download attempt now,
// given the current youtube-dl version (empty if unknown). For a waiting
// episode it also returns when the next attempt is due. Episodes that are not
// in the error state are always due.
func retryPlan(episode *model.Episode, now time.Time, version string) (retryState, time.Time) {
	if episode.Status != model.EpisodeError || episode.LastFailedAt.IsZero() {
		return retryDue, time.Time{}
	}

	if versionChanged(episode, version) {
		return retryDue, time.Time{}
	}

	if givenUp(episode) {
		return retryGivenUp, time.Time{}
	}

	next := nextAttempt(episode)
	if now.Before(next.Add(-retrySlack)) {
		return retryWaiting, next
	}

	return retryDue, time.Time{}
}

// versionChanged reports whether the episode last failed under a different
// youtube-dl version than the one installed now.
func versionChanged(episode *model.Episode, version string) bool {
	return version != "" && episode.FailedVersion != "" && version != episode.FailedVersion
}

// givenUp reports whether the episode's failures have lasted long enough to
// stop trying (until a different youtube-dl version is installed).
func givenUp(episode *model.Episode) bool {
	if episode.Retries < maxDownloadAttempts || episode.FirstFailedAt.IsZero() {
		return false
	}
	return episode.LastFailedAt.Sub(episode.FirstFailedAt) >= retryWindow
}

// nextAttempt is when a waiting episode becomes due.
func nextAttempt(episode *model.Episode) time.Time {
	return episode.LastFailedAt.Add(retryBackoff(episode.Retries))
}

// retryBackoff is the wait after the given number of failed attempts.
func retryBackoff(retries int) time.Duration {
	if retries < 1 {
		return 0
	}

	delay := retryBackoffInitial
	for i := 1; i < retries; i++ {
		delay *= 2
		if delay >= retryBackoffMax {
			return retryBackoffMax
		}
	}
	return delay
}

// recordFailure updates the episode after a failed download attempt made by
// the given youtube-dl version at the given time.
func recordFailure(episode *model.Episode, now time.Time, version string) {
	episode.Status = model.EpisodeError
	episode.Retries++
	if episode.FirstFailedAt.IsZero() {
		episode.FirstFailedAt = now
	}
	episode.LastFailedAt = now
	episode.FailedVersion = version
}

// clearFailures forgets the episode's failed attempts, after a successful
// download or a manual reset.
func clearFailures(episode *model.Episode) {
	episode.Retries = 0
	episode.FirstFailedAt = time.Time{}
	episode.LastFailedAt = time.Time{}
	episode.FailedVersion = ""
}
