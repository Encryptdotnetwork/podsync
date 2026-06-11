package builder

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
