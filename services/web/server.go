package web

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/mxpv/podsync/pkg/db"
	"github.com/mxpv/podsync/pkg/model"
)

type Server struct {
	http.Server
	db db.Storage
}

type Config struct {
	// Hostname to use for download links
	Hostname string `toml:"hostname"`
	// Port is a server port to listen to
	Port int `toml:"port"`
	// Bind a specific IP addresses for server
	// "*": bind all IP addresses which is default option
	// localhost or 127.0.0.1  bind a single IPv4 address
	BindAddress string `toml:"bind_address"`
	// Flag indicating if the server will use TLS
	TLS bool `toml:"tls"`
	// Path to a certificate file for TLS connections
	CertificatePath string `toml:"certificate_path"`
	// Path to a private key file for TLS connections
	KeyFilePath string `toml:"key_file_path"`
	// Specify path for reverse proxy and only [A-Za-z0-9]
	Path string `toml:"path"`
	// DataDir is a path to a directory to keep XML feeds and downloaded episodes,
	// that will be available to user via web server for download.
	DataDir string `toml:"data_dir"`
	// WebUIEnabled is a flag indicating if web UI is enabled
	WebUIEnabled bool `toml:"web_ui"`
	// DebugEndpoints enables /debug/vars endpoint for runtime metrics (disabled by default)
	DebugEndpoints bool `toml:"debug_endpoints"`
	// NoIndex blocks search engine indexing by serving robots.txt and adding X-Robots-Tag header (disabled by default)
	NoIndex bool `toml:"no_index"`
	// NoListing returns 404 for directory listings, only serving actual files (disabled by default)
	NoListing bool `toml:"no_listing"`
}

func New(cfg Config, storage http.FileSystem, database db.Storage) *Server {
	port := cfg.Port
	if port == 0 {
		port = 8080
	}

	bindAddress := cfg.BindAddress
	if bindAddress == "*" {
		bindAddress = ""
	}

	srv := Server{
		db: database,
	}

	srv.Addr = fmt.Sprintf("%s:%d", bindAddress, port)
	log.Debugf("using address: %s:%s", bindAddress, srv.Addr)

	// Use a custom mux instead of http.DefaultServeMux to avoid exposing
	// debug endpoints registered by imported packages (security fix for #799)
	mux := http.NewServeMux()

	fileServer := http.FileServer(storage)

	log.Debugf("handle path: /%s", cfg.Path)
	mux.Handle(fmt.Sprintf("/%s", cfg.Path), fileServer)

	// Add health check endpoint
	mux.HandleFunc("/health", srv.healthCheckHandler)

	// Thumbnail proxy: fetches external cover art server-side so the browser
	// avoids CDN hotlink restrictions (Sec-Fetch-Site: cross-site is blocked by
	// some CDNs even with referrerpolicy="no-referrer").
	mux.HandleFunc("/thumbnail", thumbnailProxyHandler)

	// Optionally enable debug endpoints (disabled by default for security)
	if cfg.DebugEndpoints {
		log.Info("debug endpoints enabled at /debug/vars")
		mux.Handle("/debug/vars", expvar.Handler())
	}

	srv.Handler = mux
	if cfg.NoIndex {
		log.Info("search engine indexing blocked (no_index enabled)")
		mux.HandleFunc("/robots.txt", robotsTxtHandler)
		srv.Handler = noIndexMiddleware(srv.Handler)
	}
	srv.Handler = securityHeadersMiddleware(srv.Handler, cfg)

	return &srv
}

// webUICSP is the Content-Security-Policy for the web UI page. Scripts load only
// from this origin (no inline script or handlers), styles and icon fonts may come
// from the pinned jsDelivr assets, and images/media stay same-origin (external
// cover art goes through /thumbnail).
const webUICSP = "default-src 'none'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; font-src https://cdn.jsdelivr.net; " +
	"img-src 'self' data:; media-src 'self'; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// securityHeadersMiddleware disables MIME sniffing on every response and applies
// the web UI's Content-Security-Policy to the page itself.
func securityHeadersMiddleware(next http.Handler, cfg Config) http.Handler {
	uiRoot := path.Clean("/" + cfg.Path)
	uiIndex := path.Join(uiRoot, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if cfg.WebUIEnabled {
			if p := path.Clean(r.URL.Path); p == uiRoot || p == uiIndex {
				w.Header().Set("Content-Security-Policy", webUICSP)
			}
		}
		next.ServeHTTP(w, r)
	})
}

type HealthStatus struct {
	Status         string    `json:"status"`
	Timestamp      time.Time `json:"timestamp"`
	FailedEpisodes int       `json:"failed_episodes,omitempty"`
	Message        string    `json:"message,omitempty"`
}

func (s *Server) healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Check for recent download failures within the last 24 hours
	failedCount := 0
	cutoffTime := time.Now().Add(-24 * time.Hour)

	// Walk through all feeds to count recent failures
	err := s.db.WalkFeeds(ctx, func(feed *model.Feed) error {
		return s.db.WalkEpisodes(ctx, feed.ID, func(episode *model.Episode) error {
			if episode.Status == model.EpisodeError && episode.PubDate.After(cutoffTime) {
				failedCount++
			}
			return nil
		})
	})

	w.Header().Set("Content-Type", "application/json")

	status := HealthStatus{
		Timestamp: time.Now(),
	}

	if err != nil {
		log.WithError(err).Error("health check database error")
		status.Status = "unhealthy"
		status.Message = "database error during health check"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else if failedCount > 0 {
		status.Status = "unhealthy"
		status.FailedEpisodes = failedCount
		status.Message = fmt.Sprintf("found %d failed downloads in the last 24 hours", failedCount)
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		status.Status = "healthy"
		status.Message = "no recent download failures detected"
		w.WriteHeader(http.StatusOK)
	}

	json.NewEncoder(w).Encode(status)
}

func robotsTxtHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("User-agent: *\nDisallow: /\n"))
}

func noIndexMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

// thumbnailAllowedHosts are CDN domains (matched by exact name or subdomain)
// that the thumbnail proxy will fetch from. Anything else is rejected.
var thumbnailAllowedHosts = []string{
	"ytimg.com", "ggpht.com", "googleusercontent.com", // YouTube
	"rumble.com", "rmbl.ws", "rumble.cloud", "1a-1791.com", // Rumble
	"odycdn.com", "odysee.com", "lbry.com", "spee.ch", // Odysee
	"vimeocdn.com", "sndcdn.com", "jtvnw.net", // Vimeo, SoundCloud, Twitch
}

func thumbnailHostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, allowed := range thumbnailAllowedHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// disallowedPrefixes are special-purpose ranges not covered by the net.IP
// helpers below: "this network", carrier-grade NAT, IETF protocol assignments,
// benchmarking, reserved space, NAT64 (which can embed private IPv4 targets)
// and 6to4.
var disallowedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
}

func isDisallowedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()
	for _, prefix := range disallowedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// safeDialContext resolves the target itself, rejects the connection if any
// resolved address is private/loopback, and dials a vetted IP directly so a
// second DNS answer can't redirect the connection elsewhere (DNS rebinding).
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	for _, ip := range ips {
		if isDisallowedIP(ip.IP) {
			return nil, fmt.Errorf("host %q resolves to disallowed address %s", host, ip.IP)
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no addresses found for host %q", host)
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// maxThumbnailBytes caps proxied image responses.
const maxThumbnailBytes = 10 << 20 // 10 MB

// thumbnailAllowedTypes are the raster formats the proxy passes through, keyed by
// the type http.DetectContentType reports for the body. Anything else (HTML, SVG,
// XML, scripts, unknown binaries) is rejected so upstream content can never be
// rendered as an active document under this origin.
var thumbnailAllowedTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// thumbnailDeclaredTypeAllowed reports whether the upstream Content-Type header is
// compatible with a raster image. Some CDNs omit it or send a generic binary type,
// so those are accepted and the body sniff decides.
func thumbnailDeclaredTypeAllowed(header string) bool {
	if header == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return false
	}
	if mediaType == "application/octet-stream" || mediaType == "binary/octet-stream" {
		return true
	}
	return strings.HasPrefix(mediaType, "image/") && mediaType != "image/svg+xml"
}

// setThumbnailSecurityHeaders stops browsers from sniffing or rendering the proxied
// bytes as anything other than the validated image type.
func setThumbnailSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

var thumbnailProxyClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{DialContext: safeDialContext},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		if !thumbnailHostAllowed(req.URL.Hostname()) {
			return fmt.Errorf("redirect to disallowed host %q", req.URL.Hostname())
		}
		return nil
	},
}

// thumbnailProxyHandler fetches an external image URL server-side and returns it to
// the browser. This bypasses CDN hotlink protection that blocks cross-origin <img>
// requests (Sec-Fetch-Site: cross-site) even when referrerpolicy="no-referrer" is set.
// Only known thumbnail CDN hosts are proxied, and connections to private/loopback
// addresses are blocked at the dialer level (SSRF protection). Only validated
// raster images are returned; the response type comes from the body, not upstream.
func thumbnailProxyHandler(w http.ResponseWriter, r *http.Request) {
	setThumbnailSecurityHeaders(w.Header())

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}

	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}

	if !thumbnailHostAllowed(parsed.Hostname()) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:150.0) Gecko/20100101 Firefox/150.0")

	resp, err := thumbnailProxyClient.Do(req)
	if err != nil {
		http.Error(w, "failed to fetch image", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream returned non-200", http.StatusBadGateway)
		return
	}

	if resp.ContentLength > maxThumbnailBytes {
		http.Error(w, "image too large", http.StatusBadGateway)
		return
	}

	if !thumbnailDeclaredTypeAllowed(resp.Header.Get("Content-Type")) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	// Sniff the leading bytes (http.DetectContentType looks at most at 512).
	head := make([]byte, 512)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		http.Error(w, "failed to read image", http.StatusBadGateway)
		return
	}
	head = head[:n]

	contentType := http.DetectContentType(head)
	if !thumbnailAllowedTypes[contentType] {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method == http.MethodHead {
		return
	}
	w.Write(head)                                                             //nolint:errcheck
	io.Copy(w, io.LimitReader(resp.Body, maxThumbnailBytes-int64(len(head)))) //nolint:errcheck
}
