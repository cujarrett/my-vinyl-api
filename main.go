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

type discogsPagination struct {
	Page    int `json:"page"`
	Pages   int `json:"pages"`
	PerPage int `json:"per_page"`
	Items   int `json:"items"`
}

type discogsCollection struct {
	Pagination discogsPagination `json:"pagination"`
	Releases   []discogsRelease  `json:"releases"`
}

// Outbound shapes

type collectionItem struct {
	ID       int    `json:"id"`
	Artist   string `json:"artist"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Label    string `json:"label"`
	CoverURL string `json:"cover_url"`
}

// collectionResponse is the envelope returned by GET /collection.
type collectionResponse struct {
	Page     int              `json:"page"`
	Pages    int              `json:"pages"`
	Items    int              `json:"items"`
	Releases []collectionItem `json:"releases"`
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

// metricsSkipPaths lists paths that are excluded from Prometheus instrumentation
// to avoid polluting metrics with noise (probes, browser auto-requests, etc.).
var metricsSkipPaths = map[string]struct{}{
	"/favicon.ico": {},
	"/health":      {},
	"/healthz":     {},
	"/robots.txt":  {},
}

// metricsMiddleware records http_requests_total and http_request_duration_seconds
// for every request passing through it. Paths in metricsSkipPaths are not
// recorded. It is a no-op when metrics are not initialised (e.g. in unit tests
// that construct app directly).
func (a *app) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.requestsTotal == nil || a.requestDuration == nil {
			next.ServeHTTP(w, r)
			return
		}
		if _, skip := metricsSkipPaths[r.URL.Path]; skip {
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
// the response into a discogsCollection.
func (a *app) fetchPage(ctx context.Context, token, url string) (discogsCollection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return discogsCollection{}, err
	}

	// Discogs requires a descriptive User-Agent — requests without one are rejected.
	req.Header.Set("User-Agent", "my-vinyl-api/1.0 +https://github.com/cujarrett/my-vinyl-api")
	req.Header.Set("Authorization", "Discogs token="+token)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return discogsCollection{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return discogsCollection{}, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

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

// corsMiddleware adds the CORS header to every response.
// allowedOrigin must be the exact origin of the SPA (scheme + hostname, no path).
func corsMiddleware(allowedOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		next.ServeHTTP(w, r)
	})
}

// healthHandler is a simple liveness probe used by Kubernetes to know the
// container is up. No logic — just return 200 OK with a JSON body.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%q}`, version) //nolint:errcheck
}

// collectionHandler proxies a single page of the Discogs collection and returns
// it as a paginated envelope. The page and per_page query params are forwarded
// directly to Discogs so the Discogs-reported pagination metadata is used as-is.
func (a *app) collectionHandler(w http.ResponseWriter, r *http.Request) {
	username := a.defaultUsername
	if q := r.URL.Query().Get("username"); q != "" {
		username = q
	}
	if username == "" {
		writeJSONError(w, "missing configuration", http.StatusInternalServerError)
		return
	}

	// Parse page param (default 1, must be >= 1).
	page := 1
	if q := r.URL.Query().Get("page"); q != "" {
		var err error
		page, err = strconv.Atoi(q)
		if err != nil || page < 1 {
			writeJSONError(w, "page must be a positive integer", http.StatusBadRequest)
			return
		}
	}

	pageURL := fmt.Sprintf(
		"%s/users/%s/collection/folders/0/releases?per_page=50&page=%d",
		a.discogsBase, url.PathEscape(username), page,
	)

	dc, err := a.fetchPage(r.Context(), a.token, pageURL)
	if err != nil {
		writeJSONError(w, "failed to fetch collection", http.StatusBadGateway)
		return
	}

	// Validate the requested page doesn't exceed the Discogs total.
	// Guard against pages=0 for empty collections by treating it as 1.
	totalPages := dc.Pagination.Pages
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		writeJSONError(w, "page out of range", http.StatusBadRequest)
		return
	}

	items := make([]collectionItem, 0, len(dc.Releases))
	for _, rel := range dc.Releases {
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
		a.collectionSize.Set(float64(dc.Pagination.Items))
	}

	resp := collectionResponse{
		Page:     dc.Pagination.Page,
		Pages:    dc.Pagination.Pages,
		Items:    dc.Pagination.Items,
		Releases: items,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("encode error: %v", err)
	}
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
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
