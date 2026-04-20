package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is set at build time via -ldflags="-X main.version=x.y.z".
// Falls back to "dev" when running locally with go run.
var version = "dev"

// Discogs API response shapes

type discogsArtist struct {
	Name string `json:"name"`
}

type discogsLabel struct {
	Name string `json:"name"`
}

type discogsBasicInfo struct {
	Title      string          `json:"title"`
	Year       int             `json:"year"`
	Artists    []discogsArtist `json:"artists"`
	Labels     []discogsLabel  `json:"labels"`
	CoverImage string          `json:"cover_image"`
}

type discogsRelease struct {
	ID               int              `json:"id"`
	BasicInformation discogsBasicInfo `json:"basic_information"`
}

type discogsPaginationURLs struct {
	Next string `json:"next"`
}

type discogsPagination struct {
	URLs discogsPaginationURLs `json:"urls"`
}

type discogsCollection struct {
	Pagination discogsPagination `json:"pagination"`
	Releases   []discogsRelease  `json:"releases"`
}

// Outbound shape

type collectionItem struct {
	ID       int    `json:"id"`
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Label    string `json:"label"`
	CoverURL string `json:"cover_url"`
}

// app holds dependencies so handlers are testable without globals.
type app struct {
	discogsBase     string
	httpClient      *http.Client
	token           string
	defaultUsername string
	collectionSize  prometheus.Gauge
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

// statusResponseWriter wraps http.ResponseWriter to capture the status code
// written by a handler so the metrics middleware can record it.
type statusResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *statusResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// metricsMiddleware records http_requests_total and http_request_duration_seconds
// for every request passing through it. It is a no-op when metrics are not
// initialised (e.g. in unit tests that construct app directly).
func (a *app) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.requestsTotal == nil || a.requestDuration == nil {
			next.ServeHTTP(w, r)
			return
		}
		rw := &statusResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rw, r)
		duration := time.Since(start).Seconds()
		a.requestsTotal.WithLabelValues(r.Method, r.URL.Path, strconv.Itoa(rw.statusCode)).Inc()
		a.requestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(duration)
	})
}

// fetchPage makes a single authenticated GET request to Discogs and decodes
// the response into a discogsCollection. The caller is responsible for
// following pagination by calling fetchPage again with the next URL.
//
// ctx is passed through so the request is cancelled if the original HTTP
// request from the SPA is cancelled (e.g. the user closes the browser tab).
func (a *app) fetchPage(ctx context.Context, token, url string) (discogsCollection, error) {
	// NewRequestWithContext attaches a context so the request respects
	// cancellation and deadlines from the caller.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return discogsCollection{}, err
	}

	// Discogs requires a descriptive User-Agent — requests without one are rejected.
	req.Header.Set("User-Agent", "my-vinyl-api/1.0 +https://github.com/cujarrett/my-vinyl-api")
	// Discogs token auth: pass the token in the Authorization header.
	req.Header.Set("Authorization", "Discogs token="+token)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return discogsCollection{}, err
	}
	// defer runs when the surrounding function returns, ensuring the response
	// body is always closed even if we return early with an error.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return discogsCollection{}, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	// Decode streams directly from the response body into the struct —
	// more memory-efficient than reading the whole body into a []byte first.
	var dc discogsCollection
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return discogsCollection{}, err
	}
	return dc, nil
}

// writeJSONError is a small helper to send a consistent JSON error body.
// Setting the header before WriteHeader is important — headers can't be
// changed after the status code is written.
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(b) //nolint:errcheck
}

// corsMiddleware wraps any handler and adds the CORS header to every response.
// allowedOrigin must be the exact origin of the SPA (scheme + hostname, no path).
// Returning a new http.HandlerFunc (which satisfies the http.Handler interface)
// is the standard Go pattern for middleware.
func corsMiddleware(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		next.ServeHTTP(w, r) // call the wrapped handler
	})
}

// healthHandler is a simple liveness probe used by Kubernetes to know the
// container is up. No logic — just return 200 OK with a JSON body.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%q}`, version) //nolint:errcheck
}

// collectionHandler fetches every page of a Discogs collection and returns
// the full list as a single flat JSON array. All pagination is handled here
// so the SPA only has to make one request.
func (a *app) collectionHandler(w http.ResponseWriter, r *http.Request) {
	// Allow the caller to override the default username via query param.
	// r.URL.Query() parses the query string; Get() returns "" if not present.
	username := a.defaultUsername
	if q := r.URL.Query().Get("username"); q != "" {
		username = q
	}
	if username == "" {
		writeJSONError(w, "missing configuration", http.StatusInternalServerError)
		return
	}

	// Build the first page URL. Discogs folder 0 = "All" — the entire collection.
	nextURL := fmt.Sprintf(
		"%s/users/%s/collection/folders/0/releases?per_page=100",
		a.discogsBase, url.PathEscape(username),
	)

	// Follow pagination until Discogs stops returning a "next" URL.
	// append on a nil slice is valid in Go — it initialises the slice for us.
	var releases []discogsRelease
	for nextURL != "" {
		page, err := a.fetchPage(r.Context(), a.token, nextURL)
		if err != nil {
			writeJSONError(w, "failed to fetch collection", http.StatusBadGateway)
			return
		}
		releases = append(releases, page.Releases...) // ... spreads the slice
		nextURL, err = a.sanitizePaginationURL(page.Pagination.URLs.Next)
		if err != nil {
			writeJSONError(w, "failed to fetch collection", http.StatusBadGateway)
			return
		}
	}

	// Trim each release down to only the fields the SPA needs.
	// make([]T, 0, n) pre-allocates capacity so append doesn't have to
	// reallocate the backing array as it grows.
	items := make([]collectionItem, 0, len(releases))
	for _, rel := range releases {
		item := collectionItem{
			ID:       rel.ID,
			Title:    rel.BasicInformation.Title,
			Year:     rel.BasicInformation.Year,
			CoverURL: rel.BasicInformation.CoverImage,
		}
		// Guard against empty slices before indexing — some releases have no artist/label.
		if len(rel.BasicInformation.Artists) > 0 {
			item.Artist = rel.BasicInformation.Artists[0].Name
		}
		if len(rel.BasicInformation.Labels) > 0 {
			item.Label = rel.BasicInformation.Labels[0].Name
		}
		items = append(items, item)
	}

	if a.collectionSize != nil {
		a.collectionSize.Set(float64(len(items)))
	}

	w.Header().Set("Content-Type", "application/json")
	// json.NewEncoder writes directly to the ResponseWriter — no intermediate buffer.
	if err := json.NewEncoder(w).Encode(items); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func (a *app) sanitizePaginationURL(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	baseURL, err := url.Parse(a.discogsBase)
	if err != nil {
		return "", err
	}
	nextURL, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !nextURL.IsAbs() {
		nextURL = baseURL.ResolveReference(nextURL)
	}
	if nextURL.Scheme != baseURL.Scheme || nextURL.Host != baseURL.Host {
		return "", fmt.Errorf("unexpected pagination URL host/scheme")
	}
	return nextURL.String(), nil
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		metricsPort = "9090"
	}

	// Set up a dedicated Prometheus registry so /metrics is never accidentally
	// served on the main port and the default global registry is not polluted.
	reg := prometheus.NewRegistry()
	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total number of HTTP requests by method, path, and status code.",
		},
		[]string{"method", "path", "status_code"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration in seconds by method and path.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path"},
	)
	collectionSize := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "discogs_collection_size",
		Help: "Number of records returned from Discogs on the most recent successful fetch.",
	})
	reg.MustRegister(requestsTotal, requestDuration, collectionSize)

	// Initialise the app with its dependencies.
	// Use a dedicated client timeout so upstream calls don't hang indefinitely.
	a := &app{
		discogsBase:     "https://api.discogs.com",
		httpClient:      &http.Client{Timeout: 15 * time.Second},
		token:           os.Getenv("DISCOGS_TOKEN"),
		defaultUsername: "cujarrett",
		collectionSize:  collectionSize,
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}
	// Fail fast at startup rather than returning errors on every request.
	if a.token == "" {
		log.Fatal("DISCOGS_TOKEN is required")
	}

	// ServeMux is Go's built-in request router. Each pattern maps to a handler function.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/collection", a.collectionHandler)

	// Wrap the whole mux: metrics outermost so every request is counted, then CORS.
	// Only the SPA origin is allowed — browsers will block requests from any other site.
	handler := a.metricsMiddleware(corsMiddleware("https://myvinyl.mattjarrett.dev", mux))

	// Start the metrics server on a separate port. This port is never publicly
	// exposed — only the in-cluster Prometheus scraper reaches it.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	metricsSrv := &http.Server{
		Addr:              ":" + metricsPort,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("metrics server listening on :%s", metricsPort)
		if err := metricsSrv.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()

	log.Printf("my-vinyl-api %s listening on :%s", version, port)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
