package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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
	defer resp.Body.Close()

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
		a.discogsBase, username,
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
		nextURL = page.Pagination.URLs.Next           // "" on the last page, ending the loop
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

	w.Header().Set("Content-Type", "application/json")
	// json.NewEncoder writes directly to the ResponseWriter — no intermediate buffer.
	if err := json.NewEncoder(w).Encode(items); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Initialise the app with its dependencies.
	// http.DefaultClient is Go's built-in HTTP client with sensible defaults.
	a := &app{
		discogsBase:     "https://api.discogs.com",
		httpClient:      http.DefaultClient,
		token:           os.Getenv("DISCOGS_TOKEN"),
		defaultUsername: "cujarrett",
	}
	// Fail fast at startup rather than returning errors on every request.
	if a.token == "" {
		log.Fatal("DISCOGS_TOKEN is required")
	}

	// ServeMux is Go's built-in request router. Each pattern maps to a handler function.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/collection", a.collectionHandler)

	// Wrap the whole mux in the CORS middleware so every route gets the header.
	// Only the SPA origin is allowed — browsers will block requests from any other site.
	handler := corsMiddleware("https://myvinyl.mattjarrett.dev", mux)

	log.Printf("my-vinyl-api %s listening on :%s", version, port)
	// ListenAndServe blocks forever serving requests. It only returns on error.
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		log.Fatal(err)
	}
}
