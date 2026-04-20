package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthHandler verifies the health endpoint returns 200 with {"status":"ok"}.
// httptest.NewRequest builds a synthetic *http.Request without opening a real socket.
// httptest.NewRecorder is a fake ResponseWriter that captures the status code,
// headers, and body written by the handler so we can assert on them.
func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Decode the response body into a generic map to check the "status" key.
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
// The app struct is constructed directly with an empty defaultUsername (zero value
// for string in Go is "") — no env vars needed.
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
// httptest.NewServer starts a real local HTTP server on a random port. We point
// the app's discogsBase at it so fetchPage hits our fake instead of real Discogs.
// ts.Client() returns an HTTP client pre-configured to trust the test server.
func TestCollectionUsernameQueryParam(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert the handler used "query-user" from the query param, not "env-user".
		if r.URL.Path != "/users/query-user/collection/folders/0/releases" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
			Releases: []discogsRelease{
				{ID: 99, BasicInformation: discogsBasicInfo{Title: "Test", Year: 2000}},
			},
		})
	}))
	defer ts.Close() // shut down the test server when the test ends

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "env-user"}
	req := httptest.NewRequest(http.MethodGet, "/collection?username=query-user", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var items []collectionItem
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != 99 {
		t.Fatalf("unexpected items: %+v", items)
	}
}

// TestCollectionPagination verifies that the handler follows the "next" URL and
// returns all releases across multiple pages as a single flat list.
func TestCollectionPagination(t *testing.T) {
	// ts must be declared with var before the handler closure so we can reference
	// ts.URL inside the closure. If we used := inside the if block, the compiler
	// would complain that ts is used before it's assigned.
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Simulate two pages: the first response includes a "next" URL pointing
		// back to this same test server with ?page=2; the second has no "next".
		if r.URL.Query().Get("page") == "2" {
			json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
				Releases: []discogsRelease{
					{ID: 2, BasicInformation: discogsBasicInfo{
						Title:   "Album Two",
						Year:    2002,
						Artists: []discogsArtist{{Name: "Artist B"}},
						Labels:  []discogsLabel{{Name: "Label B"}},
					}},
				},
			})
		} else {
			json.NewEncoder(w).Encode(discogsCollection{ //nolint:errcheck
				Pagination: discogsPagination{
					URLs: discogsPaginationURLs{Next: ts.URL + "/users/test-user/collection/folders/0/releases?per_page=100&page=2"},
				},
				Releases: []discogsRelease{
					{ID: 1, BasicInformation: discogsBasicInfo{
						Title:   "Album One",
						Year:    2001,
						Artists: []discogsArtist{{Name: "Artist A"}},
						Labels:  []discogsLabel{{Name: "Label A"}},
					}},
				},
			})
		}
	}))
	defer ts.Close()

	a := &app{discogsBase: ts.URL, httpClient: ts.Client(), token: "test-token", defaultUsername: "test-user"}

	req := httptest.NewRequest(http.MethodGet, "/collection", nil)
	w := httptest.NewRecorder()
	a.collectionHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var items []collectionItem
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatal(err)
	}
	// Both pages' releases should be merged into a single slice.
	if len(items) != 2 {
		t.Fatalf("expected 2 items across 2 pages, got %d", len(items))
	}
	if items[0].ID != 1 || items[1].ID != 2 {
		t.Fatalf("unexpected item IDs: %d, %d", items[0].ID, items[1].ID)
	}
	if items[0].Artist != "Artist A" || items[1].Artist != "Artist B" {
		t.Fatalf("unexpected artists: %q, %q", items[0].Artist, items[1].Artist)
	}
}

// TestCORSHeader verifies the middleware adds the correct Access-Control-Allow-Origin
// header to every response. We wire up the full mux + middleware to match production,
// then make a request and inspect the response headers directly.
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
