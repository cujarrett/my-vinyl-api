package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestNotFoundHandler verifies that unrecognised paths return 404 with no body.
func TestNotFoundHandler(t *testing.T) {
	for _, path := range []string{"/", "/.env", "/.git/config", "/wp-login.php", "/phpinfo.php"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		notFoundHandler(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("path %q: expected 404, got %d", path, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("path %q: expected empty body, got %q", path, w.Body.String())
		}
	}
}

// TestMetricsMiddlewareNormalizesUnknownPaths verifies that scanner/probe paths
// are all bucketed under the "unknown" label instead of their raw paths, which
// would cause a Prometheus label cardinality explosion.
func TestMetricsMiddlewareNormalizesUnknownPaths(t *testing.T) {
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_normalize_http_requests_total",
		Help: "test",
	}, []string{"method", "path", "status_code"})
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "test_normalize_http_request_duration_seconds",
		Help:    "test",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	a := &app{requestsTotal: requestsTotal, requestDuration: requestDuration}
	handler := a.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	for _, path := range []string{"/.env", "/.git/config", "/wp-login.php"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// All three unknown paths should be recorded under the single "unknown" label.
	if got := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "unknown", "404")); got != 3 {
		t.Fatalf("expected counter=3 for unknown paths, got %v", got)
	}
	// Raw paths must not appear as individual label values.
	if got := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/.env", "404")); got != 0 {
		t.Fatalf("expected counter=0 for /.env, got %v", got)
	}
}

// TestHealthHandler verifies the health endpoint returns 200 with {"status":"ok","version":"..."}.
func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
	if body["version"] == "" {
		t.Fatal("expected version field in health response")
	}
}

// TestCollectionMissingConfig verifies that a missing defaultUsername returns 500.
func TestCollectionMissingConfig(t *testing.T) {
	a := &app{discogsBase: "http://unused", httpClient: http.DefaultClient, token: "test-token"}
	req := httptest.NewRequest(http.MethodGet, "/collection", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}

// TestCollectionUsernameQueryParam verifies that ?username= overrides the default.
func TestCollectionUsernameQueryParam(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/query-user/collection/folders/0/releases" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
			Pagination: discogsPagination{Page: 1, Pages: 1, PerPage: 50, Items: 1},
			Releases: []discogsRelease{
				{ID: 99, BasicInformation: discogsBasicInfo{Title: "Test", Year: 2000}},
			},
		})
	}))
	defer ts.Close()

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "env-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection?username=query-user", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp collectionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].ID != 99 {
		t.Fatalf("unexpected releases: %+v", resp.Releases)
	}
}

// TestCollectionPagination verifies that the page param is forwarded to Discogs
// and the response is returned as a paginated envelope.
func TestCollectionPagination(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "2" {
			t.Errorf("expected page=2, got %q", r.URL.Query().Get("page"))
		}
		if r.URL.Query().Get("per_page") != "50" {
			t.Errorf("expected per_page=50, got %q", r.URL.Query().Get("per_page"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
			Pagination: discogsPagination{Page: 2, Pages: 5, PerPage: 50, Items: 120},
			Releases: []discogsRelease{
				{ID: 26, BasicInformation: discogsBasicInfo{
					Title:   "Album B",
					Year:    2002,
					Artists: []discogsArtist{{Name: "Artist B"}},
					Labels:  []discogsLabel{{Name: "Label B"}},
				}},
			},
		})
	}))
	defer ts.Close()

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "test-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection?page=2", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp collectionResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Page != 2 || resp.Pages != 5 || resp.Items != 120 {
		t.Fatalf("unexpected pagination: page=%d pages=%d items=%d", resp.Page, resp.Pages, resp.Items)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].ID != 26 {
		t.Fatalf("unexpected releases: %+v", resp.Releases)
	}
}

func TestCollectionUsernameIsEscaped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/users/evil%2Fuser/collection/folders/0/releases" {
			t.Errorf("unexpected escaped path: %s", r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{}) //nolint:errcheck
	}))
	defer ts.Close()

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "env-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection?username=evil/user", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestCORSHeader verifies the middleware adds the correct Access-Control-Allow-Origin
// header to every response.
func TestCORSHeader(t *testing.T) {
	a := &app{discogsBase: "http://unused", httpClient: http.DefaultClient}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/collection", a.collectionHandler)
	const wantOrigin = "https://myvinyl.mattjarrett.dev"
	handler := corsMiddleware(wantOrigin, mux)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != wantOrigin {
		t.Fatalf("expected CORS %q header, got %q", wantOrigin, got)
	}
}

// TestMetricsMiddlewareRecordsCounter verifies that metricsMiddleware increments
// http_requests_total with the correct method, path, and status_code labels.
func TestMetricsMiddlewareRecordsCounter(t *testing.T) {
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_http_requests_total",
		Help: "test",
	}, []string{"method", "path", "status_code"})
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "test_http_request_duration_seconds",
		Help:    "test",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	a := &app{requestsTotal: requestsTotal, requestDuration: requestDuration}
	handler := a.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/collection", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/collection", "200")); got != 1 {
		t.Fatalf("expected counter=1, got %v", got)
	}
}

// TestMetricsMiddlewareSkipsPaths verifies that requests to noise paths
// (health probes, favicon, robots.txt) are not recorded in the counter.
func TestMetricsMiddlewareSkipsPaths(t *testing.T) {
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_skip_http_requests_total",
		Help: "test",
	}, []string{"method", "path", "status_code"})
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "test_skip_http_request_duration_seconds",
		Help:    "test",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	a := &app{requestsTotal: requestsTotal, requestDuration: requestDuration}
	handler := a.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/health", "/healthz", "/favicon.ico", "/robots.txt"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	if got := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/health", "200")); got != 0 {
		t.Fatalf("/health: expected counter=0, got %v", got)
	}
	if got := testutil.ToFloat64(requestsTotal.WithLabelValues("GET", "/favicon.ico", "200")); got != 0 {
		t.Fatalf("/favicon.ico: expected counter=0, got %v", got)
	}
}

// TestCollectionSizeGaugeIsSet verifies that a successful collection fetch sets
// the discogs_collection_size gauge to the total collection size from Discogs pagination.
func TestCollectionSizeGaugeIsSet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
			Pagination: discogsPagination{Page: 1, Pages: 1, PerPage: 50, Items: 2},
			Releases: []discogsRelease{
				{ID: 1, BasicInformation: discogsBasicInfo{Title: "A", Year: 2001}},
				{ID: 2, BasicInformation: discogsBasicInfo{Title: "B", Year: 2002}},
			},
		})
	}))
	defer ts.Close()

	gauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_discogs_collection_size",
		Help: "test",
	})

	a := &app{
		discogsBase:     ts.URL,
		httpClient:      ts.Client(),
		token:           "test-token",
		defaultUsername: "test-user",
		collectionSize:  gauge,
	}

	req := httptest.NewRequest(http.MethodGet, "/collection", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := testutil.ToFloat64(gauge); got != 2 {
		t.Fatalf("expected gauge=2, got %v", got)
	}
}

// TestCollectionDefaultPagination verifies that page=1 and per_page=50 are sent
// to Discogs when no params are provided.
func TestCollectionDefaultPagination(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "1" {
			t.Errorf("expected page=1, got %q", r.URL.Query().Get("page"))
		}
		if r.URL.Query().Get("per_page") != "50" {
			t.Errorf("expected per_page=50, got %q", r.URL.Query().Get("per_page"))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
			Pagination: discogsPagination{Page: 1, Pages: 1, PerPage: 50, Items: 0},
		})
	}))
	defer ts.Close()

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "test-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// TestCollectionPageLessThan1Returns400 verifies that page=0 returns 400
// before making any upstream request.
func TestCollectionPageLessThan1Returns400(t *testing.T) {
	a := &app{discogsBase: "http://unused", httpClient: http.DefaultClient, token: "test-token", defaultUsername: "test-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection?page=0", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestCollectionPageExceedsTotalReturns400 verifies that requesting a page beyond
// the Discogs total page count returns 400.
func TestCollectionPageExceedsTotalReturns400(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
			Pagination: discogsPagination{Page: 3, Pages: 2, PerPage: 50, Items: 75},
		})
	}))
	defer ts.Close()

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "test-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection?page=3", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
