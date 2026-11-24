(() => {
  "use strict";

  const TOKEN_KEY = "podsync_scout_token";
  let authToken = sessionStorage.getItem(TOKEN_KEY) || "";
  let toastTimer = null;
  let pollTimer = null;

  function el(tag, attrs = {}, children = []) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2), v);
      else if (v !== null && v !== undefined) node.setAttribute(k, String(v));
    }
    for (const child of children) {
      if (typeof child === "string") node.appendChild(document.createTextNode(child));
      else if (child) node.appendChild(child);
    }
    return node;
  }

  function safeHttpUrl(raw) {
    try {
      const u = new URL(raw);
      return u.protocol === "http:" || u.protocol === "https:" ? u.toString() : "";
    } catch {
      return "";
    }
  }

  function formatDuration(sec) {
    const s = Math.max(0, Number(sec) || 0);
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    const r = s % 60;
    if (h > 0) return `${h}:${String(m).padStart(2, "0")}:${String(r).padStart(2, "0")}`;
    return `${m}:${String(r).padStart(2, "0")}`;
  }

  function formatSubs(n) {
    const v = Number(n) || 0;
    if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M subs`;
    if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K subs`;
    return `${v} subs`;
  }

  function formatDate(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    return d.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" });
  }

  function showToast(msg) {
    const box = document.getElementById("toast");
    box.textContent = msg;
    box.classList.remove("hidden");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => box.classList.add("hidden"), 4000);
  }

  async function api(path, options = {}) {
    const headers = {
      "Content-Type": "application/json",
      "X-Scout-Request": "1",
      ...(options.headers || {}),
    };
    if (authToken) headers["Authorization"] = `Bearer ${authToken}`;
    const resp = await fetch(`./api${path}`, { ...options, headers });
    if (resp.status === 401) {
      document.getElementById("authGate").classList.remove("hidden");
      throw new Error("Authentication required");
    }
    document.getElementById("authGate").classList.add("hidden");
    const data = await resp.json().catch(() => ({}));
    if (!resp.ok) {
      throw new Error(data.detail || data.error || `HTTP ${resp.status}`);
    }
    return data;
  }

  function policyBadge(policy) {
    if (policy === "trusted_matching") return el("span", { class: "badge badge-success", text: "Trusted (matching)" });
    if (policy === "trusted_all") return el("span", { class: "badge badge-success", text: "Trusted (all)" });
    if (policy === "blocked") return el("span", { class: "badge badge-danger", text: "Blocked" });
    return null;
  }

  function renderEpisodeCard(ep, mode) {
    const videoUrl = safeHttpUrl(ep.video_url);
    const thumbUrl = safeHttpUrl(ep.thumbnail_url);

    const thumbWrap = el("div", { class: "thumb-wrap" }, [
      thumbUrl ? el("img", { src: thumbUrl, alt: ep.title, loading: "lazy" }) : null,
      el("span", { class: "duration-tag", text: formatDuration(ep.duration_sec) }),
    ]);

    const titleNode = videoUrl
      ? el("a", { href: videoUrl, target: "_blank", rel: "noopener noreferrer", text: ep.title })
      : el("span", { text: ep.title });

    const metaRow = el("div", { class: "meta-row" }, [
      el("strong", { text: ep.channel_title || ep.channel_id }),
      el("span", { text: `• ${formatSubs(ep.subscriber_count)}` }),
      el("span", { text: `• ${formatDate(ep.published_at)}` }),
      ep.matched_person ? el("span", { class: "badge badge-accent", text: ep.matched_person }) : null,
      policyBadge(ep.channel_policy),
    ]);

    const reasonText = [ep.filter_source, ep.filter_reason].filter(Boolean).join(" — ");
    const reasonBox = reasonText
      ? el("div", { class: `reason-box ${mode === "filtered" ? "filtered" : ""}`, text: reasonText })
      : null;

    const actions = el("div", { class: "card-actions" });

    if (mode === "added") {
      actions.appendChild(
        el("button", {
          class: "btn btn-danger",
          type: "button",
          text: "Block Channel & Purge",
          onclick: async () => {
            if (!confirm(`Block "${ep.channel_title}" forever and purge its episodes from Auto Podcasts?`)) return;
            try {
              const res = await api(`/channels/${encodeURIComponent(ep.channel_id)}`, {
                method: "PUT",
                body: JSON.stringify({ policy: "blocked", channel_title: ep.channel_title, purge_existing: true }),
              });
              showToast(`Blocked ${ep.channel_title} (purged ${res.purged_video_ids.length} episode(s))`);
              await refreshAll();
            } catch (err) {
              showToast(`Error: ${err.message}`);
            }
          },
        })
      );

      if (ep.channel_policy !== "trusted_matching" && ep.channel_policy !== "trusted_all") {
        actions.appendChild(
          el("button", {
            class: "btn btn-success",
            type: "button",
            text: "Trust Channel",
            onclick: async () => {
              try {
                await api(`/channels/${encodeURIComponent(ep.channel_id)}`, {
                  method: "PUT",
                  body: JSON.stringify({ policy: "trusted_matching", channel_title: ep.channel_title }),
                });
                showToast(`Trusted ${ep.channel_title} for matching guests`);
                await refreshAll();
              } catch (err) {
                showToast(`Error: ${err.message}`);
              }
            },
          })
        );
      }

      actions.appendChild(
        el("button", {
          class: "btn btn-ghost",
          type: "button",
          text: "Remove Episode Only",
          onclick: async () => {
            try {
              await api(`/episodes/${encodeURIComponent(ep.video_id)}/purge`, { method: "POST" });
              showToast(`Removed "${ep.title}"`);
              await refreshAll();
            } catch (err) {
              showToast(`Error: ${err.message}`);
            }
          },
        })
      );
    } else {
      actions.appendChild(
        el("button", {
          class: "btn btn-primary",
          type: "button",
          text: "Grab Anyway",
          onclick: async () => {
            try {
              await api(`/episodes/${encodeURIComponent(ep.video_id)}/grab`, {
                method: "POST",
                body: JSON.stringify({ trust_channel: false }),
              });
              showToast(`Added "${ep.title}" to Auto Podcasts`);
              await refreshAll();
            } catch (err) {
              showToast(`Error: ${err.message}`);
            }
          },
        })
      );

      actions.appendChild(
        el("button", {
          class: "btn btn-success",
          type: "button",
          text: "Grab + Trust Channel",
          onclick: async () => {
            try {
              await api(`/episodes/${encodeURIComponent(ep.video_id)}/grab`, {
                method: "POST",
                body: JSON.stringify({ trust_channel: true, trust_mode: "trusted_matching" }),
              });
              showToast(`Added episode & trusted ${ep.channel_title}`);
              await refreshAll();
            } catch (err) {
              showToast(`Error: ${err.message}`);
            }
          },
        })
      );

      if (ep.channel_policy !== "blocked") {
        actions.appendChild(
          el("button", {
            class: "btn btn-danger",
            type: "button",
            text: "Block Channel",
            onclick: async () => {
              try {
                await api(`/channels/${encodeURIComponent(ep.channel_id)}`, {
                  method: "PUT",
                  body: JSON.stringify({ policy: "blocked", channel_title: ep.channel_title, purge_existing: false }),
                });
                showToast(`Blocked ${ep.channel_title}`);
                await refreshAll();
              } catch (err) {
                showToast(`Error: ${err.message}`);
              }
            },
          })
        );
      }
    }

    const body = el("div", { class: "card-body" }, [
      el("h3", { class: "card-title" }, [titleNode]),
      metaRow,
      reasonBox,
      actions,
    ]);

    return el("article", { class: "card" }, [thumbWrap, body]);
  }

  function renderEpisodeList(containerId, episodes, mode, emptyMsg) {
    const root = document.getElementById(containerId);
    root.textContent = "";
    if (!episodes.length) {
      root.appendChild(el("div", { class: "empty-state", text: emptyMsg }));
      return;
    }
    for (const ep of episodes) {
      root.appendChild(renderEpisodeCard(ep, mode));
    }
  }

  function renderPeople(people) {
    const root = document.getElementById("peopleList");
    root.textContent = "";
    if (!people.length) {
      root.appendChild(el("div", { class: "empty-state", text: "No watched people configured yet." }));
      return;
    }
    for (const p of people) {
      const info = el("div", {}, [
        el("strong", { text: p.name }),
        el("div", {
          class: "meta-text",
          text: `Query: ${p.query} • Min ${Math.round(p.min_duration_sec / 60)}m • Max age ${p.max_age_days}d`,
        }),
      ]);
      const toggleBtn = el("button", {
        class: `btn ${p.enabled ? "btn-success" : "btn-ghost"}`,
        type: "button",
        text: p.enabled ? "Enabled" : "Paused",
        onclick: async () => {
          try {
            await api(`/people/${p.id}`, {
              method: "PUT",
              body: JSON.stringify({ enabled: !p.enabled }),
            });
            await refreshRules();
          } catch (err) {
            showToast(`Error: ${err.message}`);
          }
        },
      });
      const delBtn = el("button", {
        class: "btn btn-danger",
        type: "button",
        text: "Delete",
        onclick: async () => {
          if (!confirm(`Stop watching "${p.name}"?`)) return;
          try {
            await api(`/people/${p.id}`, { method: "DELETE" });
            showToast(`Removed ${p.name}`);
            await refreshRules();
          } catch (err) {
            showToast(`Error: ${err.message}`);
          }
        },
      });
      root.appendChild(
        el("div", { class: "list-row" }, [info, el("div", { class: "card-actions" }, [toggleBtn, delBtn])])
      );
    }
  }

  function renderChannels(channels) {
    const root = document.getElementById("channelsList");
    root.textContent = "";
    if (!channels.length) {
      root.appendChild(
        el("div", {
          class: "empty-state",
          text: "No trusted or blocked channels yet. Use the buttons on episode cards to trust or block channels.",
        })
      );
      return;
    }
    for (const ch of channels) {
      const select = el("select", {
        onchange: async (e) => {
          const policy = e.target.value;
          try {
            await api(`/channels/${encodeURIComponent(ch.channel_id)}`, {
              method: "PUT",
              body: JSON.stringify({ policy, channel_title: ch.channel_title, purge_existing: policy === "blocked" }),
            });
            showToast(`Updated ${ch.channel_title} → ${policy}`);
            await refreshAll();
          } catch (err) {
            showToast(`Error: ${err.message}`);
          }
        },
      });
      for (const [val, label] of [
        ["trusted_matching", "Trust (matching guests)"],
        ["trusted_all", "Trust (all uploads)"],
        ["blocked", "Blocked"],
        ["neutral", "Neutral (remove rule)"],
      ]) {
        const opt = el("option", { value: val, text: label });
        if (ch.policy === val) opt.selected = true;
        select.appendChild(opt);
      }

      const info = el("div", {}, [
        el("strong", { text: ch.channel_title }),
        el("div", { class: "meta-text", text: `${ch.channel_id} • ${formatSubs(ch.subscriber_count)}` }),
      ]);
      root.appendChild(el("div", { class: "list-row" }, [info, select]));
    }
  }

  function scheduleScanPoll(scanning) {
    if (scanning && !pollTimer) {
      pollTimer = setInterval(async () => {
        try {
          const st = await api("/status");
          if (!st.scanning) {
            clearInterval(pollTimer);
            pollTimer = null;
            if (st.latest_scan) {
              if (st.latest_scan.status === "failed") {
                showToast(`Scan failed: ${st.latest_scan.error || "unknown error"}`);
              } else {
                showToast(
                  `Scan complete: ${st.latest_scan.added} added, ${st.latest_scan.filtered} filtered (${st.latest_scan.candidates} checked)`
                );
              }
            }
            await refreshAll();
          }
        } catch {
          clearInterval(pollTimer);
          pollTimer = null;
        }
      }, 2500);
    } else if (!scanning && pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  async function refreshStatus() {
    const st = await api("/status");
    document.getElementById("feedBadge").textContent = st.feed_id;
    document.getElementById("countAdded").textContent = String(st.counts.added);
    document.getElementById("countFiltered").textContent = String(st.counts.filtered);
    const btn = document.getElementById("scanNowBtn");
    btn.disabled = st.scanning;
    btn.textContent = st.scanning ? "Scanning…" : "Scan Now";
    scheduleScanPoll(st.scanning);
    if (st.latest_scan) {
      const ts = st.latest_scan.finished_at || st.latest_scan.started_at;
      document.getElementById("scanMeta").textContent =
        `Last scan (${st.latest_scan.status}): ${formatDate(ts)} • +${st.latest_scan.added} added, ${st.latest_scan.filtered} filtered`;
    } else {
      document.getElementById("scanMeta").textContent = "No scans run yet";
    }
  }

  async function refreshEpisodes() {
    const [addedRes, filteredRes] = await Promise.all([
      api("/episodes?status=added&limit=100"),
      api("/episodes?status=filtered&limit=100"),
    ]);
    renderEpisodeList(
      "addedList",
      addedRes.episodes,
      "added",
      "No episodes added yet. Click Scan Now to search for recent interviews."
    );
    renderEpisodeList(
      "filteredList",
      filteredRes.episodes,
      "filtered",
      "No filtered-out videos yet."
    );
  }

  async function refreshRules() {
    const [peopleRes, channelsRes, settingsRes, modelsRes] = await Promise.all([
      api("/people"),
      api("/channels"),
      api("/settings"),
      api("/ollama/models"),
    ]);
    renderPeople(peopleRes.people);
    renderChannels(channelsRes.channels);

    const s = settingsRes.settings;
    document.getElementById("ollamaEnabled").checked = Boolean(s.ollama_enabled);
    document.getElementById("fallbackMinSubs").value = String(s.fallback_min_subscribers);
    document.getElementById("scanIntervalMins").value = String(s.scan_interval_minutes);
    document.getElementById("clipDedupeDays").value = String(s.clip_dedupe_days ?? 5);

    const statusEl = document.getElementById("ollamaStatus");
    const select = document.getElementById("ollamaModel");
    select.textContent = "";

    const seenModels = new Set();
    if (modelsRes.reachable && modelsRes.models.length) {
      statusEl.textContent = "(online)";
      for (const m of modelsRes.models) {
        seenModels.add(m.name);
        const opt = el("option", { value: m.name, text: `${m.name} (${m.size_gb} GB)` });
        if (m.name === s.ollama_model) opt.selected = true;
        select.appendChild(opt);
      }
    } else {
      statusEl.textContent = "(offline)";
    }

    if (s.ollama_model && !seenModels.has(s.ollama_model)) {
      const opt = el("option", { value: s.ollama_model, text: `${s.ollama_model} (configured)` });
      opt.selected = true;
      select.appendChild(opt);
    }
  }

  async function refreshAll() {
    try {
      await Promise.all([refreshStatus(), refreshEpisodes(), refreshRules()]);
    } catch (err) {
      if (err.message !== "Authentication required") {
        showToast(`Load error: ${err.message}`);
      }
    }
  }

  // Tabs
  for (const tabBtn of document.querySelectorAll(".tab")) {
    tabBtn.addEventListener("click", () => {
      for (const b of document.querySelectorAll(".tab")) b.classList.remove("active");
      for (const p of document.querySelectorAll(".tab-panel")) p.classList.remove("active");
      tabBtn.classList.add("active");
      document.getElementById(`tab-${tabBtn.dataset.tab}`).classList.add("active");
    });
  }

  // Scan Now (non-blocking 202 + background status polling)
  document.getElementById("scanNowBtn").addEventListener("click", async () => {
    const btn = document.getElementById("scanNowBtn");
    btn.disabled = true;
    btn.textContent = "Scanning…";
    try {
      const res = await api("/scan", { method: "POST" });
      showToast(res.status === "already_running" ? "Scan already in progress…" : "Scan started in background…");
      scheduleScanPoll(true);
    } catch (err) {
      showToast(`Scan error: ${err.message}`);
      btn.disabled = false;
      btn.textContent = "Scan Now";
    }
  });

  // Add Person
  document.getElementById("addPersonForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = document.getElementById("personName").value.trim();
    const query = document.getElementById("personQuery").value.trim();
    const minMins = Number(document.getElementById("personMinMins").value) || 20;
    const maxAge = Number(document.getElementById("personMaxAge").value) || 14;
    try {
      await api("/people", {
        method: "POST",
        body: JSON.stringify({
          name,
          query: query || null,
          min_duration_sec: minMins * 60,
          max_age_days: maxAge,
        }),
      });
      e.target.reset();
      showToast(`Now watching ${name}`);
      await refreshRules();
    } catch (err) {
      showToast(`Error: ${err.message}`);
    }
  });

  // Save Settings
  document.getElementById("settingsForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    try {
      await api("/settings", {
        method: "PUT",
        body: JSON.stringify({
          ollama_enabled: document.getElementById("ollamaEnabled").checked,
          ollama_model: document.getElementById("ollamaModel").value,
          fallback_min_subscribers: Number(document.getElementById("fallbackMinSubs").value) || 0,
          scan_interval_minutes: Number(document.getElementById("scanIntervalMins").value) || 360,
          clip_dedupe_days: Math.max(0, Number(document.getElementById("clipDedupeDays").value) || 0),
        }),
      });
      showToast("Settings saved");
      await refreshRules();
    } catch (err) {
      showToast(`Error: ${err.message}`);
    }
  });

  // Optional token login
  document.getElementById("authForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    authToken = document.getElementById("authTokenInput").value.trim();
    sessionStorage.setItem(TOKEN_KEY, authToken);
    await refreshAll();
  });

  refreshAll();
})();
