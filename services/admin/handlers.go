package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	toml "github.com/pelletier/go-toml"
	"github.com/pkg/errors"

	"github.com/mxpv/podsync/pkg/config"
	"github.com/mxpv/podsync/pkg/model"
)

// maxBodyBytes caps request bodies accepted by the admin API.
const maxBodyBytes = 1 << 20 // 1 MB

// configSections are the config.toml blocks manageable through the API.
var configSections = map[string]bool{
	"server":     true,
	"storage":    true,
	"tokens":     true,
	"feeds":      true,
	"downloader": true,
	"log":        true,
}

// liveApplySections are hot-applied by the scheduler's reload loop; all other
// sections are persisted to disk but only take effect after a restart.
var liveApplySections = map[string]bool{
	"feeds":  true,
	"tokens": true,
}

type updateResponse struct {
	Status          string `json:"status"`
	ETag            string `json:"etag"`
	Applied         bool   `json:"applied"`
	RestartRequired bool   `json:"restart_required,omitempty"`
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := s.store.Get()

	w.Header().Set("ETag", s.store.ETag())
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"server":     cfg.Server,
		"storage":    cfg.Storage,
		"log":        cfg.Log,
		"database":   cfg.Database,
		"feeds":      cfg.Feeds,
		"tokens":     redactTokens(cfg.Tokens),
		"downloader": cfg.Downloader,
		"cleanup":    cfg.Cleanup,
	})
}

func (s *Server) handleGetSection(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	if !configSections[section] {
		writeError(w, http.StatusNotFound, "unknown config section")
		return
	}

	cfg := s.store.Get()

	var payload interface{}
	switch section {
	case "server":
		payload = cfg.Server
	case "storage":
		payload = cfg.Storage
	case "tokens":
		payload = redactTokens(cfg.Tokens)
	case "feeds":
		payload = cfg.Feeds
	case "downloader":
		payload = cfg.Downloader
	case "log":
		payload = cfg.Log
	}

	w.Header().Set("ETag", s.store.ETag())
	writeJSON(w, http.StatusOK, payload)
}

// handlePutSection replaces an entire config section. The section's previous
// content (including its comments) is replaced wholesale; comments in all
// other sections are preserved.
func (s *Server) handlePutSection(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	if !configSections[section] {
		writeError(w, http.StatusNotFound, "unknown config section")
		return
	}

	payload, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	frag, err := tomlFragment(payload, section)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot represent payload as TOML: "+err.Error())
		return
	}

	_, err = s.store.Update(ifMatchHeader(r), func(doc *tomledit.Document) error {
		removeKeyPrefix(doc, parser.Key{section})
		spliceFragment(doc, frag)
		return nil
	})
	if err != nil {
		writeUpdateError(w, err)
		return
	}

	live := liveApplySections[section]
	writeJSON(w, http.StatusOK, updateResponse{
		Status:          "saved",
		ETag:            s.store.ETag(),
		Applied:         live,
		RestartRequired: !live,
	})
}

func (s *Server) handleListFeeds(w http.ResponseWriter, _ *http.Request) {
	cfg := s.store.Get()

	type feedSummary struct {
		ID      string `json:"id"`
		URL     string `json:"url"`
		Format  string `json:"format"`
		Quality string `json:"quality"`
	}

	summaries := make([]feedSummary, 0, len(cfg.Feeds))
	for id, f := range cfg.Feeds {
		summaries = append(summaries, feedSummary{
			ID:      id,
			URL:     f.URL,
			Format:  string(f.Format),
			Quality: string(f.Quality),
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })

	w.Header().Set("ETag", s.store.ETag())
	writeJSON(w, http.StatusOK, summaries)
}

func (s *Server) handleGetFeed(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")

	feedConfig, ok := s.store.Get().Feeds[feedID]
	if !ok {
		writeError(w, http.StatusNotFound, "feed not found")
		return
	}

	w.Header().Set("ETag", s.store.ETag())
	writeJSON(w, http.StatusOK, feedConfig)
}

// handlePutFeed creates or replaces a single feed block.
func (s *Server) handlePutFeed(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")

	payload, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	frag, err := tomlFragment(payload, "feeds", feedID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot represent payload as TOML: "+err.Error())
		return
	}

	_, err = s.store.Update(ifMatchHeader(r), func(doc *tomledit.Document) error {
		removeKeyPrefix(doc, parser.Key{"feeds", feedID})
		spliceFragment(doc, frag)
		return nil
	})
	if err != nil {
		writeUpdateError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, updateResponse{
		Status:  "saved",
		ETag:    s.store.ETag(),
		Applied: true,
	})
}

func (s *Server) handleDeleteFeed(w http.ResponseWriter, r *http.Request) {
	feedID := r.PathValue("id")

	if _, ok := s.store.Get().Feeds[feedID]; !ok {
		writeError(w, http.StatusNotFound, "feed not found")
		return
	}

	_, err := s.store.Update(ifMatchHeader(r), func(doc *tomledit.Document) error {
		removeKeyPrefix(doc, parser.Key{"feeds", feedID})
		return nil
	})
	if err != nil {
		writeUpdateError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, updateResponse{
		Status:  "deleted",
		ETag:    s.store.ETag(),
		Applied: true,
	})
}

func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	if err := s.reload(); err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, updateResponse{
		Status:  "reloaded",
		ETag:    s.store.ETag(),
		Applied: true,
	})
}

// decodeBody parses a JSON object body, converting json.Number values to
// int64/float64 so the TOML encoder emits proper integer literals.
func decodeBody(r *http.Request) (map[string]interface{}, error) {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.UseNumber()

	var payload map[string]interface{}
	if err := dec.Decode(&payload); err != nil {
		return nil, err
	}

	normalizeJSON(payload)
	return payload, nil
}

func normalizeJSON(v interface{}) interface{} {
	switch t := v.(type) {
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case map[string]interface{}:
		for k, val := range t {
			t[k] = normalizeJSON(val)
		}
		return t
	case []interface{}:
		for i, val := range t {
			t[i] = normalizeJSON(val)
		}
		return t
	default:
		return v
	}
}

// tomlFragment renders payload as a TOML document rooted at the given key path
// (e.g. path "feeds", "ID1" produces a [feeds.ID1] table).
func tomlFragment(payload map[string]interface{}, path ...string) (*tomledit.Document, error) {
	wrapped := payload
	for i := len(path) - 1; i >= 0; i-- {
		wrapped = map[string]interface{}{path[i]: wrapped}
	}

	tree, err := toml.TreeFromMap(wrapped)
	if err != nil {
		return nil, err
	}

	text, err := tree.ToTomlString()
	if err != nil {
		return nil, err
	}

	return tomledit.Parse(strings.NewReader(text))
}

// removeKeyPrefix removes every section and key-value mapping whose full key
// path starts with prefix, including nested tables (e.g. prefix {feeds, ID1}
// also removes [feeds.ID1.filters]).
func removeKeyPrefix(doc *tomledit.Document, prefix parser.Key) {
	hasPrefix := func(name parser.Key) bool {
		if len(name) < len(prefix) {
			return false
		}
		for i, part := range prefix {
			if name[i] != part {
				return false
			}
		}
		return true
	}

	filterItems := func(sectionName parser.Key, items []parser.Item) []parser.Item {
		var kept []parser.Item
		for _, item := range items {
			if kv, ok := item.(*parser.KeyValue); ok {
				full := append(append(parser.Key{}, sectionName...), kv.Name...)
				if hasPrefix(full) {
					continue
				}
			}
			kept = append(kept, item)
		}
		return kept
	}

	if doc.Global != nil {
		doc.Global.Items = filterItems(nil, doc.Global.Items)
	}

	var kept []*tomledit.Section
	for _, section := range doc.Sections {
		if section.Heading != nil && hasPrefix(section.Heading.Name) {
			continue
		}
		var name parser.Key
		if section.Heading != nil {
			name = section.Heading.Name
		}
		section.Items = filterItems(name, section.Items)
		kept = append(kept, section)
	}
	doc.Sections = kept
}

// spliceFragment appends the fragment's content to the document. Sections that
// already exist in the document (e.g. the [feeds] parent table when adding a
// new [feeds.X]) are merged into the existing section rather than appended,
// since TOML forbids duplicate table headers.
func spliceFragment(doc, frag *tomledit.Document) {
	if frag.Global != nil && len(frag.Global.Items) > 0 {
		if doc.Global == nil {
			doc.Global = frag.Global
		} else {
			doc.Global.Items = append(doc.Global.Items, frag.Global.Items...)
		}
	}

	existing := make(map[string]*tomledit.Section)
	for _, section := range doc.Sections {
		if section.Heading != nil {
			existing[keyString(section.Heading.Name)] = section
		}
	}

	for _, section := range frag.Sections {
		if section.Heading != nil {
			if present, ok := existing[keyString(section.Heading.Name)]; ok {
				present.Items = append(present.Items, section.Items...)
				continue
			}
			existing[keyString(section.Heading.Name)] = section
		}
		doc.Sections = append(doc.Sections, section)
	}
}

func keyString(k parser.Key) string {
	return strings.Join(k, ".")
}

func redactTokens(tokens map[model.Provider]config.StringSlice) map[string][]string {
	redacted := make(map[string][]string, len(tokens))
	for provider, list := range tokens {
		keys := make([]string, 0, len(list))
		for _, token := range list {
			keys = append(keys, redactToken(token))
		}
		redacted[string(provider)] = keys
	}
	return redacted
}

// redactToken hides all but the last 4 characters of an API key.
func redactToken(token string) string {
	if len(token) <= 4 {
		return "****"
	}
	return "****" + token[len(token)-4:]
}

func ifMatchHeader(r *http.Request) string {
	return strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
}

// writeUpdateError maps store.Update failures to HTTP statuses: 412 for ETag
// conflicts, 500 for persistence failures, 400 for everything else
// (validation, malformed payloads).
func writeUpdateError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrConflict):
		writeError(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, config.ErrWriteFailed):
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
