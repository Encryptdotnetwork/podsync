// Podsync admin page. Plain script, no third-party code. All dynamic content is
// written with textContent, never as HTML. The token is typed by the user,
// kept in sessionStorage (cleared when the tab closes) and sent only in the
// Authorization header of same-origin requests.
(function () {
  'use strict';

  var TOKEN_KEY = 'podsync_admin_token';
  var POLL_MS = 2000;

  var STATE_LABELS = {
    idle: 'Idle',
    queued: 'Queued',
    syncing: 'Syncing',
    completed: 'Completed',
    no_new_episodes: 'No new episodes',
    failed: 'Failed',
    rate_limited: 'Rate limited'
  };

  var ERROR_LABELS = {
    source: 'The platform could not be reached or the feed could not be resolved.',
    database: 'The episode database could not be read.',
    download: 'An episode could not be downloaded or stored.',
    feed_build: 'The feed file could not be written.',
    internal: 'Unexpected internal error. See the server log.',
    cancelled: 'Interrupted by a shutdown.'
  };

  var REFUSAL_LABELS = {
    rate_limited: 'is rate limited by the platform',
    too_soon: 'was synced manually a moment ago'
  };

  var el = {
    login: document.getElementById('login'),
    loginForm: document.getElementById('login-form'),
    loginError: document.getElementById('login-error'),
    token: document.getElementById('token'),
    panel: document.getElementById('panel'),
    feeds: document.getElementById('feeds'),
    empty: document.getElementById('empty'),
    message: document.getElementById('message'),
    syncAll: document.getElementById('sync-all'),
    refresh: document.getElementById('refresh'),
    signOut: document.getElementById('sign-out')
  };

  var pollTimer = null;

  function getToken() {
    return sessionStorage.getItem(TOKEN_KEY) || '';
  }

  function setMessage(text) {
    el.message.textContent = text || '';
  }

  function showLogin(errorText) {
    stopPolling();
    el.panel.hidden = true;
    el.login.hidden = false;
    el.loginError.textContent = errorText || '';
    el.token.value = '';
    el.token.focus();
  }

  function showPanel() {
    el.login.hidden = true;
    el.panel.hidden = false;
    el.loginError.textContent = '';
  }

  function stopPolling() {
    if (pollTimer !== null) {
      clearTimeout(pollTimer);
      pollTimer = null;
    }
  }

  // api performs a same-origin request with the bearer token. It resolves to
  // {status, headers, body}; a 401 signs the user out.
  function api(method, path) {
    return fetch(path, {
      method: method,
      headers: { 'Authorization': 'Bearer ' + getToken() },
      credentials: 'omit',
      cache: 'no-store',
      referrerPolicy: 'no-referrer'
    }).then(function (response) {
      if (response.status === 401) {
        sessionStorage.removeItem(TOKEN_KEY);
        showLogin('The token was rejected. Enter it again.');
        throw new Error('unauthorized');
      }
      return response.json().catch(function () { return null; }).then(function (body) {
        return { status: response.status, headers: response.headers, body: body };
      });
    });
  }

  function formatTime(iso) {
    if (!iso) { return ''; }
    var date = new Date(iso);
    return isNaN(date.getTime()) ? '' : date.toLocaleString();
  }

  function plural(count, word) {
    return count + ' ' + word + (count === 1 ? '' : 's');
  }

  function resultText(feed) {
    if (feed.state === 'idle' || feed.state === 'queued' || feed.state === 'syncing') {
      return '';
    }
    if (feed.state === 'failed' || feed.state === 'rate_limited') {
      return '';
    }
    return plural(feed.discovered, 'new episode') + ', ' +
      feed.downloaded + ' downloaded, ' + feed.failed + ' failed';
  }

  function detailText(feed) {
    if (feed.state === 'failed') {
      return ERROR_LABELS[feed.error] || 'The update failed.';
    }
    if (feed.state === 'rate_limited') {
      var when = formatTime(feed.retry_after);
      return when ? 'Next attempt possible after ' + when + '.' : 'Rate limited by the platform.';
    }
    if (feed.state === 'syncing' && feed.pending) {
      return 'Another run is queued next.';
    }
    if (feed.state === 'queued') {
      return 'Waiting for its turn.';
    }
    return '';
  }

  function cell(row, text, className) {
    var td = document.createElement('td');
    if (className) { td.className = className; }
    td.textContent = text;
    row.appendChild(td);
    return td;
  }

  function renderFeed(feed) {
    var row = document.createElement('tr');

    cell(row, feed.id, 'feed-id');

    var stateCell = cell(row, STATE_LABELS[feed.state] || feed.state, 'state state-' + feed.state);
    stateCell.setAttribute('data-state', feed.state);

    cell(row, feed.trigger === 'manual' ? 'Sync now' : (feed.trigger === 'scheduled' ? 'Schedule' : ''), 'muted');
    cell(row, formatTime(feed.finished_at), 'muted');
    cell(row, resultText(feed));
    cell(row, detailText(feed), 'muted');

    var actions = document.createElement('td');
    var button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Sync now';
    button.disabled = feed.state === 'queued' || (feed.state === 'syncing' && feed.pending);
    button.addEventListener('click', function () { syncFeed(feed.id); });
    actions.appendChild(button);
    row.appendChild(actions);

    return row;
  }

  function render(status) {
    var feeds = status.feeds || [];

    while (el.feeds.firstChild) {
      el.feeds.removeChild(el.feeds.firstChild);
    }
    feeds.forEach(function (feed) { el.feeds.appendChild(renderFeed(feed)); });

    el.empty.hidden = feeds.length > 0;
    el.syncAll.disabled = feeds.length === 0;
  }

  function schedulePoll(busy) {
    stopPolling();
    if (busy) {
      pollTimer = setTimeout(refresh, POLL_MS);
    }
  }

  function refresh() {
    return api('GET', 'sync').then(function (result) {
      if (result.status !== 200 || !result.body) {
        setMessage('Could not load the status (HTTP ' + result.status + ').');
        schedulePoll(false);
        return;
      }
      showPanel();
      render(result.body);
      schedulePoll(!!result.body.busy);
    }).catch(function (err) {
      if (err && err.message === 'unauthorized') { return; }
      setMessage('Could not reach the server.');
      schedulePoll(false);
    });
  }

  function retryText(headers) {
    var seconds = parseInt(headers.get('Retry-After'), 10);
    if (isNaN(seconds)) { return ''; }
    return seconds >= 120 ? ' Try again in ' + Math.ceil(seconds / 60) + ' minutes.' : ' Try again in ' + seconds + ' seconds.';
  }

  function syncFeed(feedID) {
    setMessage('');
    return api('POST', 'feeds/' + encodeURIComponent(feedID) + '/sync').then(function (result) {
      if (result.status === 202) {
        setMessage('Sync queued for ' + feedID + '.');
      } else if (result.status === 404) {
        setMessage('Feed ' + feedID + ' no longer exists.');
      } else if (result.status === 429) {
        var reason = result.body && REFUSAL_LABELS[result.body.reason];
        setMessage('Feed ' + feedID + ' ' + (reason || 'cannot be synced right now') + '.' + retryText(result.headers));
      } else {
        setMessage('The request failed (HTTP ' + result.status + ').');
      }
      return refresh();
    }).catch(function (err) {
      if (err && err.message === 'unauthorized') { return; }
      setMessage('Could not reach the server.');
    });
  }

  function syncAll() {
    setMessage('');
    return api('POST', 'sync').then(function (result) {
      var body = result.body || {};
      if (result.status === 202) {
        var queued = (body.queued || []).length;
        var refused = (body.refused || []).length;
        var text = 'Sync queued for ' + plural(queued, 'feed') + '.';
        if (refused > 0) {
          text += ' ' + plural(refused, 'feed') + ' skipped (rate limited or synced a moment ago).';
        }
        setMessage(text);
      } else if (result.status === 429) {
        setMessage('No feed can be synced right now.' + retryText(result.headers));
      } else {
        setMessage('The request failed (HTTP ' + result.status + ').');
      }
      return refresh();
    }).catch(function (err) {
      if (err && err.message === 'unauthorized') { return; }
      setMessage('Could not reach the server.');
    });
  }

  el.loginForm.addEventListener('submit', function (event) {
    event.preventDefault();
    var value = el.token.value.trim();
    if (!value) { return; }
    sessionStorage.setItem(TOKEN_KEY, value);
    el.token.value = '';
    setMessage('');
    refresh();
  });

  el.syncAll.addEventListener('click', syncAll);
  el.refresh.addEventListener('click', function () { setMessage(''); refresh(); });
  el.signOut.addEventListener('click', function () {
    sessionStorage.removeItem(TOKEN_KEY);
    setMessage('');
    showLogin('');
  });

  if (getToken()) {
    refresh();
  } else {
    showLogin('');
  }
})();
