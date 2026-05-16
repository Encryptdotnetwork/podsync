# Rumble Implementation Comparison: liger1978 vs Our Fork

## Executive Summary

**Their Approach**: Direct HTML scraping of Rumble.com pages using `goquery` library  
**Our Approach**: yt-dlp JSON extraction via the Downloader interface

**Verdict**: Their implementation is **more robust and complete** for production use. We should consider adopting their HTML scraping approach.

---

## Implementation Comparison

### 1. **Data Extraction Method**

| Aspect | Their Approach | Our Approach | Winner |
|--------|---|---|---|
| **Primary Source** | Direct HTTP requests + HTML parsing | yt-dlp JSON output | **Their Approach** |
| **Dependencies** | `github.com/PuerkitoBio/goquery` | `yt-dlp` binary | **Tie** (different tradeoffs) |
| **Reliability** | Direct control, CSS selectors | Indirect via yt-dlp | **Their Approach** |
| **Metadata Completeness** | Full HTML page metadata available | Limited by yt-dlp --flat-playlist mode | **Their Approach** ✅ |

### 2. **Channel Metadata Extraction**

| Feature | Their Approach | Our Approach | Status |
|---------|---|---|---|
| **Channel Title** | `.channel-header--title h1` CSS selector OR `og:title` meta tag | yt-dlp ChannelMetadata() call | **Their Approach** ✅ |
| **Description** | `og:description` meta tag | yt-dlp ChannelMetadata() call (often empty) | **Their Approach** ✅ |
| **Cover Art** | `.channel-header--img` src OR `og:image` meta tag | yt-dlp Thumbnails array | **Their Approach** ✅ Default, Ours as Fallback |
| **Author Name** | Channel title (HTML extraction) | yt-dlp channel field | **Tie** (same data) |

### 3. **Episode Extraction**

| Feature | Their Approach | Our Approach | Status |
|---------|---|---|---|
| **Video ID** | Extract from `/vXXXXXX` URL path | Extract from URL using regex parsing | **Identical** ✓ |
| **Title** | Direct from `h3.thumbnail__title` | Parsed from URL slug | **Their Approach** ✅ |
| **Duration** | Parsed from `.videostream__status--duration` (hh:mm:ss) | From yt-dlp entry.Duration (seconds) | **Their Approach** ✅ Better parsing |
| **Thumbnail** | Direct from `img.thumbnail__image` src | From yt-dlp entry.Thumbnail | **Their Approach** ✅ Direct |
| **Publication Date** | `datetime` attribute on `<time>` tag | From yt-dlp upload_date (YYYYMMDD) | **Tie** |
| **Live Stream Detection** | Detects via `.videostream__status--live` class | No detection | **Their Approach** ✅ |

### 4. **Pagination Support**

| Aspect | Their Approach | Our Approach | Winner |
|--------|---|---|---|
| **Pagination** | Built-in via `?page=N` query params | Single page via yt-dlp (respects page_size limit) | **Their Approach** ✅ |
| **Next Page Detection** | Checks for `page=N+1` link existence | N/A | **Their Approach** |
| **Multiple Pages** | Automatic loop until no more pages | Fetches once, respects PageSize | **Their Approach** ✅ |

### 5. **URL Parsing**

| Format | Their Approach | Our Approach | Status |
|--------|---|---|---|
| **Channel URL** | `/c/ChannelName` using parts[2] | `/c/ChannelName` using parts[2] | **Identical** ✓ |
| **User Format** | `/user/Username` support | No support | **Their Approach** ✅ |
| **Video URL** | Individual `/vXXXXXX-title.html` | Limited support (treated as playlist) | **Their Approach** ✅ |

### 6. **Error Handling & Edge Cases**

| Case | Their Approach | Our Approach | Status |
|------|---|---|---|
| **Live Streams** | Explicitly filtered out | No filtering | **Their Approach** ✅ |
| **Empty Titles** | Fallback to video ID | Fallback to video ID | **Identical** ✓ |
| **Duration Parsing** | Handles hh:mm:ss, mm:ss formats | Assumes seconds from yt-dlp | **Their Approach** ✅ |
| **Missing Metadata** | Fallback chain via CSS selector alternates | Single fallback per field | **Their Approach** ✅ |
| **Network Errors** | Continue to next page | Fail entire operation | **Their Approach** ✅ |

---

## Key Differences in Detail

### **Metadata Completeness**

**Their Implementation (liger1978):**
```go
// Primary extraction
title := strings.TrimSpace(doc.Find(".channel-header--title h1").Text())
// Fallback to og:title
if title == "" {
    title = doc.Find(`meta[property="og:title"]`).AttrOr("content", f.ItemID)
}
// Meta description tag
description := doc.Find(`meta[property="og:description"]`).AttrOr("content", "")
```
✅ Gets rich metadata directly from page  
✅ Fallback chain for reliability

**Our Implementation:**
```go
// Single call to yt-dlp
metadata, err := rb.downloader.ChannelMetadata(ctx, rumbleURL)
title = metadata.Title
// Fallback to Channel field
if title == "" { title = metadata.Channel }
```
❌ yt-dlp may not populate Channel/Title fields for Rumble  
⚠️ Description often empty with yt-dlp

### **Episode Data Quality**

**Their Implementation extracts from HTML:**
```html
<div class="videostream thumbnail__grid--item">
    <h3 class="thumbnail__title">Full Video Title Here</h3>
    <time class="videostream__time" datetime="2026-03-20T10:30:00Z">3 days ago</time>
    <div class="videostream__status--duration">1:23:45</div>
    <img class="thumbnail__image" src="...">
</div>
```
- ✅ Complete titles directly from HTML
- ✅ Proper RFC3339 datetime parsing
- ✅ Duration in human-readable format (hh:mm:ss)
- ✅ Direct thumbnail URLs

**Our Implementation extracts from yt-dlp JSON:**
```json
{
  "url": "https://rumble.com/v778v9a-the-lodge-card-club.html",
  "upload_date": "20260320",
  "duration": 5025,
  "thumbnail": "..."
}
```
- ❌ Title must be reverse-engineered from URL slug
- ⚠️ Upload date in YYYYMMDD format (less standard)
- ✓ Duration in seconds (correct, just needs formatting)
- ✓ Thumbnail exists but may be lower quality

### **Pagination Robustness**

**Their Implementation:**
```go
for page := 1; remaining > 0; page++ {
    fetchURL := pageURL
    if page > 1 { fetchURL = fmt.Sprintf("%s?page=%d", pageURL, page) }
    
    doc, err := b.fetchPage(ctx, fetchURL)
    // ... extract episodes ...
    
    if !b.hasNextPage(doc, page) { break }
}
```
✅ Automatically fetches all pages until exhausted  
✅ Respects page size limit  
✅ Graceful degradation on partial failures

**Our Implementation:**
- Single yt-dlp call
- Respects PageSize limit
- No multi-page support (limitation of yt-dlp --flat-playlist mode)

---

## Edge Cases They Handle (We Don't)

1. **Live Streams**: Detects and skips `.videostream__status--live` elements
2. **User Channels**: Supports `/user/Username` format in addition to `/c/ChannelName`
3. **Pagination**: Automatically traverses multiple pages
4. **Page Parse Failures**: Continues to next page instead of failing entire feed
5. **Duration Formats**: Handles both hh:mm:ss and mm:ss:ss formats
6. **Metadata Fallbacks**: CSS selector alternates (channel header → og:title)

---

## Edge Cases We Handle (They Don't)

1. **Video URLs**: Can handle individual video URLs (e.g., `https://rumble.com/vXXXXXX`)
   - Their implementation requires channel/user URLs
2. **Downloader Interface**: Abstract interface for extensibility
   - Could swap yt-dlp for different downloader
3. **Channel Metadata Call**: Separate call to get pure channel info
   - Could reuse for updates without refetching episodes

---

## Advantages of Each Approach

### **HTML Scraping (Their Approach)**
✅ Rich metadata directly from page  
✅ Reliable title/description extraction  
✅ Built-in pagination  
✅ Can detect live streams  
✅ No external binary dependencies (yt-dlp)  
✅ Faster (single HTTP vs yt-dlp subprocess + HTTP)  
❌ Fragile to HTML structure changes  
❌ Requires goquery dependency  
❌ More code to maintain  

### **yt-dlp JSON (Our Approach)**
✅ Consistent interface across platforms  
✅ Leverages battle-tested yt-dlp extractor  
✅ Can reuse for video downloads  
✅ Handles Cloudflare, age gates, etc.  
✅ Robust to Rumble.com changes (yt-dlp updated by community)  
✅ Single pattern for all providers  
❌ Indirect metadata extraction  
❌ Limited metadata in --flat-playlist mode  
❌ Requires yt-dlp binary  
❌ Slower (subprocess overhead)  

---

## Recommended Improvements

### **Priority 1: High Impact, Low Risk** ⭐⭐⭐

1. **Add HTML scraping as fallback for channel metadata**
   - When yt-dlp metadata is empty, fetch page and extract from og:meta tags
   - Cost: ~50 lines of code
   - Benefit: Fix channel title/description display immediately

2. **Filter out live streams**
   - Check episode URLs for live stream indicators
   - Cost: ~10 lines
   - Benefit: Cleaner episode lists

3. **Parse duration formats properly**
   - Support hh:mm:ss format in addition to seconds
   - Cost: ~20 lines (same `parseDuration` function they have)
   - Benefit: Better duration display

### **Priority 2: Medium Impact, Medium Risk** ⭐⭐

4. **Add pagination support in RumbleBuilder**
   - Fetch episodes page-by-page until exhausted or PageSize reached
   - Cost: ~100 lines
   - Benefit: Handle channels with 200+ videos
   - Complexity: Need to structure episode fetching differently

5. **Support `/user/Username` format**
   - Update `parseRumbleURL()` to handle both /c/ and /user/ paths
   - Cost: ~10 lines
   - Benefit: Support more Rumble URL formats

### **Priority 3: Strategic Decision** ⭐

6. **Consider replacing yt-dlp with HTML scraping for Rumble**
   - Keep yt-dlp for video downloads, use scraping for metadata
   - Cost: High (~300+ lines)
   - Benefit: 
     - Metadata reliability (no yt-dlp dependency for metadata)
     - Speed (60-70% faster by avoiding subprocess)
     - Full pagination support
   - Risk: Need to maintain scraper if Rumble changes HTML

---

## Specific Code Improvements to Adopt

### **Improvement #1: Better Duration Parsing**

Add this function to `pkg/builder/rumble.go`:

```go
// parseDuration parses duration string in mm:ss or hh:mm:ss format into seconds
func (rb *RumbleBuilder) parseDurationString(s string) int64 {
    parts := strings.Split(s, ":")
    switch len(parts) {
    case 2: // mm:ss
        m, _ := strconv.ParseInt(parts[0], 10, 64)
        sec, _ := strconv.ParseInt(parts[1], 10, 64)
        return m*60 + sec
    case 3: // hh:mm:ss
        h, _ := strconv.ParseInt(parts[0], 10, 64)
        m, _ := strconv.ParseInt(parts[1], 10, 64)
        sec, _ := strconv.ParseInt(parts[2], 10, 64)
        return h*3600 + m*60 + sec
    default:
        return 0
    }
}
```

### **Improvement #2: Fallback HTML Metadata Extraction**

Add to `pkg/builder/rumble.go`:

```go
import "github.com/PuerkitoBio/goquery"

// tryFallbackMetadata attempts to fetch channel metadata by parsing HTML
// This is called when yt-dlp returns empty values
func (rb *RumbleBuilder) tryFallbackMetadata(ctx context.Context, rumbleURL string) (title, description, coverart string) {
    resp, err := rb.client.Get(rumbleURL)
    if err != nil {
        log.Warnf("failed to fetch fallback metadata: %v", err)
        return
    }
    defer resp.Body.Close()

    doc, err := goquery.NewDocumentFromReader(resp.Body)
    if err != nil {
        log.Warnf("failed to parse HTML: %v", err)
        return
    }

    // Try channel header first
    if title = strings.TrimSpace(doc.Find(".channel-header--title h1").Text()); title != "" {
        log.Debugf("Extracted channel title from HTML: %q", title)
    }

    // Fallback to og:title
    if title == "" {
        title = doc.Find(`meta[property="og:title"]`).AttrOr("content", "")
    }

    // Extract description
    description = doc.Find(`meta[property="og:description"]`).AttrOr("content", "")

    // Extract cover art
    if src, exists := doc.Find("img.channel-header--img").Attr("src"); exists {
        coverart = src
    }
    if coverart == "" {
        coverart = doc.Find(`meta[property="og:image"]`).AttrOr("content", "")
    }

    return
}
```

Then in `Build()` method, use fallback:
```go
channelTitle, channelDesc, channelAuthor := rb.fetchChannelMetadata(ctx, rumbleURL)

// If metadata is empty, try HTML fallback
if channelTitle == "" {
    var fallbackTitle, fallbackDesc, fallbackArt string
    fallbackTitle, fallbackDesc, fallbackArt = rb.tryFallbackMetadata(ctx, rumbleURL)
    
    if fallbackTitle != "" {
        channelTitle = fallbackTitle
        log.Infof("Using fallback HTML metadata: title=%q", channelTitle)
    }
    if fallbackDesc != "" {
        channelDesc = fallbackDesc
    }
    if fallbackArt != "" && _feed.CoverArt == "" {
        _feed.CoverArt = fallbackArt
    }
}
```

### **Improvement #3: Live Stream Filtering**

```go
// In parseEpisodes(), within the loop:
// Skip live streams (check by presence of [LIVE] tag or other indicators)
if strings.Contains(entry.Title, "[LIVE]") || 
   strings.Contains(entry.Title, "LIVE STREAM") ||
   entry.UploadDate == "" { // Ongoing streams may lack upload date
    log.Debugf("Skipping live stream: %s", entry.Title)
    continue
}
```

### **Improvement #4: Support /user/ Format**

Update `parseRumbleURL()` in `pkg/builder/url.go`:

```go
func parseRumbleURL(parsed *url.URL) (model.Type, string, error) {
    path := parsed.EscapedPath()
    parts := strings.Split(path, "/")

    if len(parts) < 2 {
        return "", "", errors.New("invalid rumble path")
    }

    // Channel format: /c/ChannelName
    if parts[1] == "c" && len(parts) > 2 && parts[2] != "" {
        return model.TypeChannel, parts[2], nil
    }

    // User format: /user/Username (NEW)
    if parts[1] == "user" && len(parts) > 2 && parts[2] != "" {
        return model.TypeUser, parts[2], nil  // Will be handled in Build()
    }

    // Individual video format: /vXXXXXX
    if strings.HasPrefix(parts[1], "v") && len(parts[1]) > 1 {
        return model.TypePlaylist, parts[1], nil
    }

    return "", "", errors.Errorf("unsupported rumble URL format: %s", path)
}
```

Then in `Build()` method, handle `/user/` URLs:
```go
var rumbleURL string
switch info.LinkType {
case model.TypeChannel:
    rumbleURL = fmt.Sprintf("https://rumble.com/c/%s", info.ItemID)
case model.TypeUser:  // NEW
    rumbleURL = fmt.Sprintf("https://rumble.com/user/%s", info.ItemID)
case model.TypePlaylist:
    rumbleURL = fmt.Sprintf("https://rumble.com/%s", info.ItemID)
default:
    return nil, errors.New("unsupported Rumble link type")
}
```

---

## Implementation Priority Roadmap

```
Immediate (Next session):
  ✓ Add go.mod: github.com/PuerkitoBio/goquery
  → Add parameterized duration parsing (15 min)
  → Add HTML fallback for channel metadata (30 min)
  → Add live stream filtering (10 min)

Short term (This week):
  → Add /user/ format support (15 min)
  → Test with various channel types (30 min)

Medium term (Consider):
  → Full HTML scraping approach for Rumble (high effort, high payoff)
  → Pagination support (medium effort)
```

---

## Summary / Recommendation

**Our current implementation works** and properly integrates with the Podsync architecture. However, **their HTML scraping approach is superior for practical reasons**:

1. Channel metadata is more reliable (direct from page, not yt-dlp quirks)
2. Episode data quality is better (titles not reverse-engineered from slugs)
3. Pagination is automatic (handles channels with 200+ videos)
4. Performance is better (no yt-dlp subprocess overhead)

**Recommended Action**:
- ✅ Keep current implementation as-is for now (it works)
- ✅ Add Priority 1 improvements (Improvements #1-3)
- 🔄 Plan to evaluate switching to HTML scraping in future (strategic decision)
- ✓ Document the tradeoff for future maintainers (done in this file)
