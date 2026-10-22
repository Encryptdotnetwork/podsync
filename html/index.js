/*
 * Podsync web UI.
 *
 * Feed and episode metadata comes from third-party platforms, so it is treated
 * as untrusted: everything is rendered with DOM APIs and textContent, URLs are
 * parsed and scheme-checked before being assigned, and no markup is built from
 * strings. The page's Content-Security-Policy only allows this same-origin
 * script, so inline handlers are not used anywhere.
 */
'use strict';

const WEB_SCHEMES = ['http:', 'https:'];

/**
 * Parses a URL (relative URLs resolve against the page) and returns its
 * absolute form if it uses an allowed scheme, otherwise ''.
 * @param {string} url
 * @param {string[]} schemes - allowed protocols, e.g. ['https:']
 * @returns {string}
 */
function safeUrl(url, schemes = WEB_SCHEMES) {
    if (!url) return '';
    try {
        const parsed = new URL(url, window.location.href);
        return schemes.includes(parsed.protocol) ? parsed.href : '';
    } catch (e) {
        return '';
    }
}

/**
 * Creates an element with an optional class name and text content.
 * @param {string} tag
 * @param {string} [className]
 * @param {string} [text]
 * @returns {HTMLElement}
 */
function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined && text !== null) node.textContent = text;
    return node;
}

/**
 * Creates a Bootstrap icon element.
 * @param {string} name - icon class, e.g. 'bi-play-fill'
 * @param {string} [extra] - additional classes
 * @returns {HTMLElement}
 */
function icon(name, extra) {
    return el('i', 'bi ' + name + (extra ? ' ' + extra : ''));
}

/**
 * Creates a link that opens in a new tab, or null if the URL is not allowed.
 * @param {string} url
 * @param {string} className
 * @returns {HTMLAnchorElement|null}
 */
function externalLink(url, className) {
    const href = safeUrl(url);
    if (!href) return null;
    const link = el('a', className);
    link.href = href;
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
    return link;
}

/**
 * Converts possibly HTML-formatted description text to plain text. DOMParser
 * documents are inert: scripts do not run and resources are not loaded.
 * @param {string} text
 * @returns {string}
 */
function toPlainText(text) {
    if (!text) return '';
    const doc = new DOMParser().parseFromString(text, 'text/html');
    return (doc.body.textContent || '').replace(/\s+/g, ' ').trim();
}

/**
 * Loads OPML XML from a URL and converts it to a JavaScript dictionary
 * @param {string} url - The URL containing the OPML XML
 * @returns {Promise<Object>} A promise that resolves to the OPML data as a dictionary
 */
async function loadOPMLFromURL(url) {
    try {
        // Fetch the OPML XML from the URL
        const response = await fetch(url);

        if (!response.ok) {
            throw new Error(`Error loading OPML. HTTP status: ${response.status}`);
        }

        const xmlText = await response.text();

        // Parse the XML
        const parser = new DOMParser();
        const xmlDoc = parser.parseFromString(xmlText, 'text/xml');

        // Check for parsing errors
        const parserError = xmlDoc.querySelector('parsererror');
        if (parserError) {
            throw new Error('OPML XML parsing error: ' + parserError.textContent);
        }

        // Convert OPML to dictionary
        return parseOPMLToDict(xmlDoc);

    } catch (error) {
        console.error('Error loading OPML:', error);
        throw error;
    }
}

/**
 * Parses OPML XML document to a JavaScript dictionary
 * @param {Document} xmlDoc - The parsed XML document
 * @returns {Object} The OPML data as a dictionary
 */
function parseOPMLToDict(xmlDoc) {
    const opml = xmlDoc.querySelector('opml');
    if (!opml) {
        throw new Error('Invalid OPML: no opml element found');
    }

    const result = {
        version: opml.getAttribute('version') || '1.0',
        head: {},
        body: []
    };

    // Parse head section
    const head = opml.querySelector('head');
    if (head) {
        for (const child of head.children) {
            result.head[child.tagName] = child.textContent;
        }
    }

    // Parse body section
    const body = opml.querySelector('body');
    if (body) {
        result.body = parseOutlines(body.querySelectorAll(':scope > outline'));
    }

    return result;
}

/**
 * Recursively parses outline elements
 * @param {NodeList} outlines - The outline elements to parse
 * @returns {Array} Array of outline objects
 */
function parseOutlines(outlines) {
    const result = [];

    for (const outline of outlines) {
        const item = {};

        // Get all attributes
        for (const attr of outline.attributes) {
            item[attr.name] = attr.value;
        }

        // Check for child outlines
        const childOutlines = outline.querySelectorAll(':scope > outline');
        if (childOutlines.length > 0) {
            item.children = parseOutlines(childOutlines);
        }

        result.push(item);
    }

    return result;
}

// Route external thumbnail URLs through the server-side proxy so the
// browser avoids CDN hotlink restrictions (Sec-Fetch-Site: cross-site).
// Only http(s) URLs are used; anything else yields no image.
function proxyThumbnail(url) {
    const href = safeUrl(url);
    if (!href) return '';
    if (new URL(href).origin === window.location.origin) return href;
    return '/thumbnail?url=' + encodeURIComponent(href);
}

// Rewrite Podsync-generated absolute URLs (feed XML, episode media) to
// the origin the dashboard is currently served from. The server bakes
// server.hostname into the OPML and RSS, so a dashboard opened via a
// different origin (LAN IP vs. reverse-proxy domain) would otherwise
// hit mixed-content/CORS blocks on every fetch. The result is always
// same-origin, so it can never point at another scheme or site.
function relativizeServerUrl(url) {
    if (!url) return '';
    try {
        const parsed = new URL(url, window.location.origin);
        if (!WEB_SCHEMES.includes(parsed.protocol)) return '';
        return window.location.origin + parsed.pathname + parsed.search;
    } catch (e) {
        return '';
    }
}

/**
 * Fetches and parses RSS feed to extract episodes
 * @param {string} feedUrl - The RSS feed URL
 * @returns {Promise<Array>} Array of episode objects
 */
async function fetchEpisodesFromFeed(feedUrl) {
    try {
        const response = await fetch(feedUrl);

        if (!response.ok) {
            throw new Error(`HTTP error! status: ${response.status}`);
        }

        const rssText = await response.text();
        const parser = new DOMParser();
        const rssDoc = parser.parseFromString(rssText, 'text/xml');

        // Check for parsing errors
        const parserError = rssDoc.querySelector('parsererror');
        if (parserError) {
            throw new Error('RSS parsing error: ' + parserError.textContent);
        }

        return parseRSSEpisodes(rssDoc, feedUrl);

    } catch (error) {
        console.error(`Error fetching feed ${feedUrl}:`, error);
        return []; // Return empty array on error to continue processing other feeds
    }
}

/**
 * Parses RSS XML to extract episode information
 * @param {Document} rssDoc - The parsed RSS XML document
 * @param {string} feedUrl - The original feed URL for reference
 * @returns {Array} Array of episode objects
 */
function parseRSSEpisodes(rssDoc, feedUrl) {
    const episodes = [];
    const channel = rssDoc.querySelector('channel');

    if (!channel) {
        console.warn(`No channel found in RSS feed: ${feedUrl}`);
        return episodes;
    }

    // Get feed metadata
    const feedTitle = channel.querySelector('title')?.textContent || 'Unknown Feed';
    const feedLink = channel.querySelector('link')?.textContent || '';

    // Get all items (episodes)
    const items = channel.querySelectorAll('item');

    items.forEach(item => {
        const enclosure = item.querySelector('enclosure');
        const episode = {
            feedUrl: feedUrl,
            feedTitle: feedTitle,
            feedLink: feedLink,
            title: item.querySelector('title')?.textContent || 'Untitled Episode',
            description: item.querySelector('description')?.textContent || '',
            link: item.querySelector('link')?.textContent || '',
            pubDate: item.querySelector('pubDate')?.textContent || '',
            guid: item.querySelector('guid')?.textContent || '',
            author: item.querySelector('author')?.textContent ||
                   item.querySelector('itunes\\:author, author[itunes]')?.textContent || '',
            duration: item.querySelector('itunes\\:duration, duration[itunes]')?.textContent || '',
            image: proxyThumbnail(item.querySelector('itunes\\:image, image[itunes]')?.getAttribute('href') ||
                   channel.querySelector('itunes\\:image, image[itunes]')?.getAttribute('href') ||
                   channel.querySelector('image url')?.textContent || ''),
            enclosureUrl: relativizeServerUrl(enclosure?.getAttribute('url') || ''),
            enclosureType: enclosure?.getAttribute('type') || '',
            enclosureLength: enclosure?.getAttribute('length') || ''
        };

        // Parse publication date
        if (episode.pubDate) {
            episode.pubDateParsed = new Date(episode.pubDate);
            // Fallback if date parsing fails
            if (isNaN(episode.pubDateParsed.getTime())) {
                episode.pubDateParsed = new Date(0); // Unix epoch as fallback
            }
        } else {
            episode.pubDateParsed = new Date(0);
        }

        episodes.push(episode);
    });

    return episodes;
}

/**
 * Fetches all episodes from OPML feeds and sorts them by publication date
 * @param {Object} opmlData - The parsed OPML data
 * @returns {Promise<Array>} Array of all episodes sorted by publication date (newest first)
 */
async function getAllEpisodesSortedByDate(opmlData) {
    const allEpisodes = [];
    const feedUrls = extractFeedUrls(opmlData.body);

    console.log(`Found ${feedUrls.length} feeds to process...`);

    // Fetch episodes from all feeds concurrently
    const episodePromises = feedUrls.map(async (feedData) => {
        console.log(`Fetching episodes from: ${feedData.title}`);
        return fetchEpisodesFromFeed(feedData.xmlUrl);
    });

    // Wait for all feeds to be processed
    const episodeArrays = await Promise.all(episodePromises);

    // Flatten the array of arrays
    episodeArrays.forEach(episodes => {
        allEpisodes.push(...episodes);
    });

    // Sort by publication date (newest first)
    allEpisodes.sort((a, b) => b.pubDateParsed.getTime() - a.pubDateParsed.getTime());

    console.log(`Total episodes found: ${allEpisodes.length}`);
    return allEpisodes;
}

/**
 * Recursively extracts feed URLs from OPML body structure
 * @param {Array} outlines - OPML outline array
 * @returns {Array} Array of feed objects with title and xmlUrl
 */
function extractFeedUrls(outlines) {
    const feeds = [];

    outlines.forEach(outline => {
        // If this outline has an xmlUrl, it's a feed. The URL is
        // rewritten to the current origin so the dashboard works no
        // matter which hostname/IP it was opened through.
        const xmlUrl = relativizeServerUrl(outline.xmlUrl);
        if (xmlUrl) {
            feeds.push({
                title: outline.title || outline.text || 'Unknown Feed',
                xmlUrl: xmlUrl,
                htmlUrl: outline.htmlUrl || '',
                description: outline.description || ''
            });
        }

        // Recursively check children
        if (outline.children && outline.children.length > 0) {
            feeds.push(...extractFeedUrls(outline.children));
        }
    });

    return feeds;
}

// UI-specific functions
let allEpisodes = [];

/**
 * Loads episodes and displays them in the UI
 */
async function loadEpisodes() {
    const loading = document.getElementById('loading');
    const errorMessage = document.getElementById('error-message');
    const episodesContainer = document.getElementById('episodes-container');
    const episodeCount = document.getElementById('episode-count');

    // Show loading, hide others
    loading.classList.remove('d-none');
    errorMessage.classList.add('d-none');
    episodesContainer.classList.add('d-none');

    try {
        const opmlData = await loadOPMLFromURL('podsync.opml');

        allEpisodes = await getAllEpisodesSortedByDate(opmlData);

        // Update episode count
        episodeCount.textContent = `${allEpisodes.length} episodes loaded`;

        // Display all episodes
        displayEpisodesInUI(allEpisodes);

        // Hide loading, show episodes
        loading.classList.add('d-none');
        episodesContainer.classList.remove('d-none');

    } catch (error) {
        console.error('Error loading episodes:', error);

        // Show error
        loading.classList.add('d-none');
        document.getElementById('error-text').textContent = error.message;
        errorMessage.classList.remove('d-none');
    }
}

/**
 * Builds the play button, or a disabled placeholder when there is no media.
 * @param {Object} episode
 * @returns {HTMLElement}
 */
function buildPlayControl(episode) {
    if (!episode.enclosureUrl) {
        const placeholder = el('div', 'play-button');
        placeholder.style.opacity = '0.5';
        placeholder.title = 'No audio available';
        placeholder.appendChild(icon('bi-music-note'));
        return placeholder;
    }

    const button = el('button', 'play-button');
    button.type = 'button';
    button.title = 'Play episode';
    button.appendChild(icon('bi-play-fill'));
    button.addEventListener('click', () => togglePlay(button));
    return button;
}

/**
 * Builds the audio element for an episode (hidden until played).
 * @param {Object} episode
 * @returns {HTMLAudioElement}
 */
function buildAudioPlayer(episode) {
    const audio = el('audio', 'audio-player d-none');
    audio.controls = true;
    audio.preload = 'none';

    const source = document.createElement('source');
    source.src = episode.enclosureUrl;
    source.type = /^[\w.+-]+\/[\w.+-]+$/.test(episode.enclosureType) ? episode.enclosureType : 'audio/mpeg';
    audio.appendChild(source);
    audio.appendChild(document.createTextNode('Your browser does not support the audio element.'));
    return audio;
}

/**
 * Builds the cover image (or placeholder) column for an episode.
 * @param {Object} episode
 * @returns {DocumentFragment}
 */
function buildImage(episode) {
    const fragment = document.createDocumentFragment();
    const placeholder = el('div', 'episode-image-placeholder');
    placeholder.appendChild(icon('bi-image'));

    if (episode.image) {
        const img = el('img', 'episode-image');
        img.src = episode.image;
        img.alt = episode.title;
        img.addEventListener('error', () => {
            img.style.display = 'none';
            placeholder.style.display = 'flex';
        });
        placeholder.style.display = 'none';
        fragment.appendChild(img);
    }
    fragment.appendChild(placeholder);
    return fragment;
}

/**
 * Builds one episode card.
 * @param {Object} episode
 * @returns {HTMLElement}
 */
function buildEpisodeCard(episode) {
    const date = episode.pubDateParsed.toLocaleDateString();
    const time = episode.pubDateParsed.toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'});
    const plainDesc = toPlainText(episode.description);
    const shortDesc = plainDesc ?
        (plainDesc.length > 200 ? plainDesc.substring(0, 200) + '...' : plainDesc) :
        'No description available';

    const card = el('div', 'card episode-card mb-3');
    const body = el('div', 'card-body');
    const row = el('div', 'row align-items-start');
    card.appendChild(body);
    body.appendChild(row);

    // Controls column: play button and download link
    const controlsCol = el('div', 'col-auto');
    const controls = el('div', 'controls-section d-flex flex-column align-items-center gap-2');
    controls.appendChild(buildPlayControl(episode));
    if (episode.enclosureUrl) {
        const download = externalLink(episode.enclosureUrl, 'text-success');
        if (download) {
            download.style.fontSize = '1.2rem';
            download.title = 'Download Audio';
            download.appendChild(icon('bi-download'));
            controls.appendChild(download);
        }
    }
    controlsCol.appendChild(controls);
    row.appendChild(controlsCol);

    // Main column: metadata, title, description, player
    const mainCol = el('div', 'col');
    const meta = el('div', 'd-flex flex-wrap align-items-center mb-2');

    const feedBadge = externalLink(episode.feedLink, 'badge feed-badge me-2') || el('span', 'badge feed-badge me-2');
    feedBadge.textContent = episode.feedTitle;
    meta.appendChild(feedBadge);

    const rss = externalLink(episode.feedUrl, 'text-warning me-2');
    if (rss) {
        rss.style.fontSize = '0.9rem';
        rss.title = 'RSS Feed';
        rss.appendChild(icon('bi-rss'));
        meta.appendChild(rss);
    }

    if (episode.author) {
        meta.appendChild(el('small', 'episode-meta me-2', 'by ' + episode.author));
    }

    if (episode.duration) {
        const duration = el('span', 'badge duration-badge me-2');
        duration.appendChild(icon('bi-clock', 'me-1'));
        duration.appendChild(document.createTextNode(episode.duration));
        meta.appendChild(duration);
    }

    const published = el('small', 'episode-meta');
    published.appendChild(icon('bi-calendar', 'me-1'));
    published.appendChild(document.createTextNode(`${date} ${time}`));
    meta.appendChild(published);
    mainCol.appendChild(meta);

    const heading = el('h5', 'card-title mb-2');
    const title = externalLink(episode.link, 'episode-title') || el('span', 'episode-title');
    title.textContent = episode.title;
    heading.appendChild(title);
    mainCol.appendChild(heading);

    mainCol.appendChild(el('p', 'description-text mb-2', shortDesc));

    if (episode.enclosureUrl) {
        mainCol.appendChild(buildAudioPlayer(episode));
    }
    row.appendChild(mainCol);

    // Image column
    const imageCol = el('div', 'col-auto');
    imageCol.appendChild(buildImage(episode));
    row.appendChild(imageCol);

    return card;
}

/**
 * Displays episodes in the UI
 * @param {Array} episodes - Episodes to display
 */
function displayEpisodesInUI(episodes) {
    const container = document.getElementById('episodes-container');
    container.replaceChildren(...episodes.map(buildEpisodeCard));
}

/**
 * Toggles audio playback for the card containing the button
 * @param {HTMLElement} button - The play button element
 */
function togglePlay(button) {
    const audioPlayer = button.closest('.card-body').querySelector('.audio-player');
    const icon = button.querySelector('i');

    if (!audioPlayer.dataset.endedListener) {
        // Reset the button when playback finishes
        audioPlayer.addEventListener('ended', function() {
            icon.className = 'bi bi-play-fill';
            button.title = 'Play episode';
        });
        audioPlayer.dataset.endedListener = 'true';
    }

    if (audioPlayer.classList.contains('d-none')) {
        // Show player and start playing
        audioPlayer.classList.remove('d-none');
        audioPlayer.play();
        icon.className = 'bi bi-pause-fill';
        button.title = 'Pause episode';
    } else if (audioPlayer.paused) {
        audioPlayer.play();
        icon.className = 'bi bi-pause-fill';
        button.title = 'Pause episode';
    } else {
        audioPlayer.pause();
        icon.className = 'bi bi-play-fill';
        button.title = 'Play episode';
    }
}

// Load episodes when page loads
document.addEventListener('DOMContentLoaded', function() {
    loadEpisodes();
});
