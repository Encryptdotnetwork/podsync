package builder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mxpv/podsync/pkg/feed"
	"github.com/mxpv/podsync/pkg/model"
	"github.com/mxpv/podsync/pkg/ytdl"
)

// htmlFromLogs is a representative sample matching the structure seen in the
// HTMX API debug log for a 1-video playlist response.
const htmlFromLogs = `
		<li		class="videostream__details min-w-0"
	data-js="videostream_details"
	data-video-id="436113600"		>
					<div class="videostream__list-index"></div>
					<div
		id=b5CO7OKzuUM-436113600 hx-preserve		class="videostream videostream__list-item"
		role="listitem"		data-video-id="436113600"
	>
				<div
		class="thumbnail__thumb"
	>
			<img
	class="thumbnail__image "
	draggable="false"
	src="https://hugh.cdn.rumble.cloud/video/fwe2/5e/s8/1/i/h/P/n/ihPnA.oq1b-small-Trump-and-XI-The-Alliance-T..jpg"
	alt="Trump &amp; XI The Alliance That Will Finally DESTROY The City of London"
	onerror="this.onerror=null;this.src=&#34;data:image/svg+xml,%3Csvg width=&#39;480&#39; height=&#39;270&#39; xmlns=&#39;http://www.w3.org/2000/svg&#39;/%3E&#34;"
	width="480"
	height="270"
	loading="lazy"			>
				<div class="videostream__info videostream__info--bottom">


		<div
			class="videostream__badge videostream__status videostream__status--duration"
		>
			56:42		</div>
	</div>
			<a class="videostream__link link" draggable="false" href="/v79u3ew-trump-and-xi-the-alliance-that-will-finally-destroy-the-city-of-london.html?playlist_id=b5CO7OKzuUM" ></a>
	</div>
			<div class="videostream__footer">
					<a class="title__link link" href="/v79u3ew-trump-and-xi-the-alliance-that-will-finally-destroy-the-city-of-london.html?playlist_id=b5CO7OKzuUM" >
			<h3
				class="thumbnail__title line-clamp-2"
				title="Trump &amp; XI The Alliance That Will Finally DESTROY The City of London"
			>
				Trump &amp; XI The Alliance That Will Finally DESTROY The City of London			</h3>
		</a>
	</div>
	</div>
</li>
`

func TestExtractRumblePlaylistEntries_RealHTML(t *testing.T) {
	entries := extractRumblePlaylistEntries([]byte(htmlFromLogs))

	require.Len(t, entries, 1, "expected exactly one entry")

	e := entries[0]
	assert.Equal(t, "https://rumble.com/v79u3ew-trump-and-xi-the-alliance-that-will-finally-destroy-the-city-of-london.html", e.URL)
	assert.Equal(t, "https://hugh.cdn.rumble.cloud/video/fwe2/5e/s8/1/i/h/P/n/ihPnA.oq1b-small-Trump-and-XI-The-Alliance-T..jpg", e.Thumbnail)
	assert.NotEmpty(t, e.Title, "Title should not be empty")
}

func TestExtractRumblePlaylistEntries_TwoVideos(t *testing.T) {
	html2 := htmlFromLogs + `
<li class="videostream__details min-w-0" data-video-id="999">
<div class="videostream videostream__list-item" data-video-id="999">
<div class="thumbnail__thumb">
<img
class="thumbnail__image "
src="https://example.com/thumb2.jpg"
loading="lazy">
<a class="videostream__link link" href="/v99zzzzz-second-video.html?playlist_id=abc" ></a>
</div>
<div class="videostream__footer">
<a class="title__link link" href="/v99zzzzz-second-video.html?playlist_id=abc">
<h3
class="thumbnail__title line-clamp-2"
title="Second Video Title"
>Second Video Title</h3>
</a>
</div>
</div>
</li>
`
	entries := extractRumblePlaylistEntries([]byte(html2))
	require.Len(t, entries, 2)

	assert.Contains(t, entries[0].URL, "/v79u3ew-")
	assert.NotEmpty(t, entries[0].Thumbnail)
	assert.NotEmpty(t, entries[0].Title)

	assert.Equal(t, "https://rumble.com/v99zzzzz-second-video.html", entries[1].URL)
	assert.Equal(t, "https://example.com/thumb2.jpg", entries[1].Thumbnail)
	assert.Equal(t, "Second Video Title", entries[1].Title)
}

func TestExtractRumblePlaylistEntries_EmptyHTML(t *testing.T) {
	entries := extractRumblePlaylistEntries([]byte("<html></html>"))
	assert.Empty(t, entries)
}

// A video missing its thumbnail must not shift the thumbnails/titles of the
// videos that follow it (regression test for positional zip association).
func TestExtractRumblePlaylistEntries_MissingThumbnailNoMisalignment(t *testing.T) {
	html3 := htmlFromLogs + `
<li class="videostream__details min-w-0" data-video-id="999">
<div class="videostream videostream__list-item" data-video-id="999">
<div class="thumbnail__thumb">
<a class="videostream__link link" href="/v99zzzzz-second-video.html?playlist_id=abc" ></a>
</div>
<div class="videostream__footer">
<a class="title__link link" href="/v99zzzzz-second-video.html?playlist_id=abc">
<h3 class="thumbnail__title line-clamp-2" title="Second Video Title">Second Video Title</h3>
</a>
</div>
</div>
</li>
<li class="videostream__details min-w-0" data-video-id="1000">
<div class="videostream videostream__list-item" data-video-id="1000">
<div class="thumbnail__thumb">
<img
class="thumbnail__image "
src="https://example.com/thumb3.jpg"
loading="lazy">
<a class="videostream__link link" href="/v10aaaaa-third-video.html?playlist_id=abc" ></a>
</div>
<div class="videostream__footer">
<a class="title__link link" href="/v10aaaaa-third-video.html?playlist_id=abc">
<h3 class="thumbnail__title line-clamp-2" title="Third Video Title">Third Video Title</h3>
</a>
</div>
</div>
</li>
`
	entries := extractRumblePlaylistEntries([]byte(html3))
	require.Len(t, entries, 3)

	// Second video has no thumbnail: its field stays empty
	assert.Equal(t, "https://rumble.com/v99zzzzz-second-video.html", entries[1].URL)
	assert.Empty(t, entries[1].Thumbnail)
	assert.Equal(t, "Second Video Title", entries[1].Title)

	// Third video must keep its OWN thumbnail and title, not inherit shifted ones
	assert.Equal(t, "https://rumble.com/v10aaaaa-third-video.html", entries[2].URL)
	assert.Equal(t, "https://example.com/thumb3.jpg", entries[2].Thumbnail)
	assert.Equal(t, "Third Video Title", entries[2].Title)
}

// rumblePlaylistItem renders one playlist item the way the HTMX endpoint does:
// thumbnail, two identical hrefs, title attribute, then the upload <time>.
func rumblePlaylistItem(slug, title, thumb, datetime string) string {
	item := `<li class="videostream__details min-w-0">
<div class="thumbnail__thumb">
<img
	class="thumbnail__image "
	draggable="false"
	src="` + thumb + `">
<a class="videostream__link link" href="/` + slug + `.html?playlist_id=abc" ></a>
</div>
<a class="title__link link" href="/` + slug + `.html?playlist_id=abc">
<h3
	class="thumbnail__title line-clamp-2"
	title="` + title + `"
>` + title + `</h3>
</a>`
	if datetime != "" {
		item += `
<span class="videostream__data--item videostream__date">
<time
	class="videostream__data--subitem videostream__time"
	datetime="` + datetime + `"
>5 months ago</time>
</span>`
	}
	return item + "\n</li>\n"
}

func TestExtractRumblePlaylistEntries_DecodesEntitiesAndParsesTimes(t *testing.T) {
	body := rumblePlaylistItem("v79xihw-first", "They&#39;re Here &amp; Now", "https://example.com/a.jpg?w=1&amp;h=2", "2026-05-16T07:58:12-04:00") +
		rumblePlaylistItem("v79u3li-second", "Trump & XI", "https://example.com/b.jpg", "") +
		rumblePlaylistItem("v79fe8o-third", "Third", "https://example.com/c.jpg", "2026-05-05T09:30:55-04:00")

	entries := extractRumblePlaylistEntries([]byte(body))
	require.Len(t, entries, 3)

	assert.Equal(t, "They're Here & Now", entries[0].Title)
	assert.Equal(t, "https://example.com/a.jpg?w=1&h=2", entries[0].Thumbnail)
	assert.Equal(t, time.Date(2026, 5, 16, 11, 58, 12, 0, time.UTC), entries[0].Published)

	// A bare ampersand is left alone, and a missing <time> must not borrow the next item's
	assert.Equal(t, "Trump & XI", entries[1].Title)
	assert.True(t, entries[1].Published.IsZero())

	assert.Equal(t, time.Date(2026, 5, 5, 13, 30, 55, 0, time.UTC), entries[2].Published)
}

func TestParseRumbleOGTag_DecodesEntities(t *testing.T) {
	body := []byte(`<meta property="og:title" content="Rock &amp; Roll &#39;26">` +
		`<meta content="It&#39;s here" property="og:description">`)

	assert.Equal(t, "Rock & Roll '26", parseRumbleOGTag(body, "title"))
	assert.Equal(t, "It's here", parseRumbleOGTag(body, "description"))
}

// Undated flat entries must get distinct, strictly older timestamps in list
// order, so the feed keeps the newest-first order and keep_last is deterministic.
func TestRumbleParseEpisodes_UndatedEntriesKeepListOrder(t *testing.T) {
	metadata := ytdl.PlaylistMetadata{Entries: []ytdl.PlaylistEntry{
		{Url: "https://rumble.com/v111aaa-newest.html"},
		{Url: "https://rumble.com/v222bbb-middle.html", UploadDate: "20260501"},
		{Url: "https://rumble.com/v333ccc-oldest.html"},
	}}
	feedModel := &model.Feed{PageSize: 10}

	rb := &RumbleBuilder{}
	require.NoError(t, rb.parseEpisodes(context.Background(), &feed.Config{}, feedModel, metadata))
	require.Len(t, feedModel.Episodes, 3)

	assert.True(t, feedModel.Episodes[0].PubDate.After(feedModel.Episodes[2].PubDate))
	assert.Equal(t, time.Second, feedModel.Episodes[0].PubDate.Sub(feedModel.Episodes[2].PubDate)/2)
	assert.Equal(t, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), feedModel.Episodes[1].PubDate)
}

type fakeRumbleDownloader struct {
	url   string
	limit int
}

func (f *fakeRumbleDownloader) PlaylistMetadata(context.Context, string) (ytdl.PlaylistMetadata, error) {
	return ytdl.PlaylistMetadata{}, errors.New("PlaylistMetadata must not be used for Rumble channels")
}

func (f *fakeRumbleDownloader) PlaylistEntries(_ context.Context, url string, limit int) (ytdl.PlaylistMetadata, error) {
	f.url, f.limit = url, limit
	return ytdl.PlaylistMetadata{
		Title:   "Channel Title",
		Channel: "channel-name",
		Entries: []ytdl.PlaylistEntry{{Url: "https://rumble.com/v111aaa-first-video.html"}},
	}, nil
}

// Channel feeds fetch only page_size entries in one yt-dlp call instead of
// enumerating the whole channel on every update.
func TestRumbleBuild_ChannelFetchesOnlyPageSizeEntries(t *testing.T) {
	downloader := &fakeRumbleDownloader{}
	rb, err := NewRumbleBuilder(downloader)
	require.NoError(t, err)

	result, err := rb.Build(context.Background(), &feed.Config{URL: "https://rumble.com/c/somechannel", PageSize: 5})
	require.NoError(t, err)

	assert.Equal(t, "https://rumble.com/c/somechannel", downloader.url)
	assert.Equal(t, 5, downloader.limit)
	assert.Equal(t, "Channel Title", result.Title)
	assert.Equal(t, "channel-name", result.Author)
	require.Len(t, result.Episodes, 1)
	assert.Equal(t, "First Video", result.Episodes[0].Title)
}
