package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubEndpoint fakes the sidecar Manager: no process management, the base URL
// points at an httptest server.
type stubEndpoint struct {
	baseURL string
	token   string
	ensured int
}

func (s *stubEndpoint) Ensure(_ context.Context) error { s.ensured++; return nil }
func (s *stubEndpoint) BaseURL() string                { return s.baseURL }
func (s *stubEndpoint) Token() string                  { return s.token }
func (s *stubEndpoint) Touch()                         {}

func newStubSidecar(t *testing.T, handler http.HandlerFunc) (*SidecarProvider, *stubEndpoint) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	ep := &stubEndpoint{baseURL: server.URL, token: "test-token"}
	return NewSidecarProvider(ep), ep
}

// A 500 {"error":...} from /posts must become a Go error carrying the sidecar
// message — never an empty PostsPage (the legacy miss-scan root cause).
func TestSidecarPosts500BecomesError(t *testing.T) {
	provider, _ := newStubSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sidecar-Token") != "test-token" {
			t.Errorf("missing/incorrect X-Sidecar-Token header")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"posts: upstream returned no aweme_list (risk control or expired cookie?)"}`))
	})

	page, err := provider.PostsPage(context.Background(), "secX", "", 20)
	if err == nil {
		t.Fatalf("got page %+v with nil error; a 500 must never yield an empty result", page)
	}
	if page != nil {
		t.Fatalf("page must be nil on error, got %+v", page)
	}
	if !strings.Contains(err.Error(), "upstream returned no aweme_list") {
		t.Fatalf("error must carry the sidecar message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error must carry the HTTP status, got: %v", err)
	}
}

// Any non-200 status is an error, and the error preserves the body context.
func TestSidecarAllNon200AreErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusForbidden, `{"error":"invalid or missing sidecar token"}`, "invalid or missing sidecar token"},
		{http.StatusServiceUnavailable, `{"error":"cookie not configured"}`, "cookie not configured"},
		{http.StatusBadGateway, `not json at all`, "not json at all"},
	}
	for _, tc := range cases {
		provider, _ := newStubSidecar(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := provider.Profile(context.Background(), "secX")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: got err %v, want it to contain %q", tc.status, err, tc.want)
		}
	}
}

// Happy path: a 200 response decodes into the typed structs.
func TestSidecarHappyPath(t *testing.T) {
	provider, ep := newStubSidecar(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/profile":
			if r.URL.Query().Get("sec_uid") != "secQ" {
				t.Errorf("sec_uid query = %q", r.URL.Query().Get("sec_uid"))
			}
			_, _ = w.Write([]byte(`{"sec_uid":"secQ","nickname":"博主","avatar_url":"http://a","aweme_count":42,"signature":"s"}`))
		case "/posts":
			next := "120"
			_, _ = w.Write([]byte(fmt.Sprintf(`{"items":[{"item_id":"v1","title":"t","cover_url":"c","duration":45000,"published_at":"2026-01-01T00:00:00Z","mix_id":null,"mix_name":null}],"has_more":true,"next_cursor":%q}`, next)))
		case "/work":
			_, _ = w.Write([]byte(`{"item_id":"v1","title":"t","cover_url":"c","duration":45000,"variants":[{"quality":"1080p","width":1080,"height":1920,"bitrate":1,"size_bytes":2,"url":"http://v"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ctx := context.Background()

	profile, err := provider.Profile(ctx, "secQ")
	if err != nil || profile.Nickname != "博主" || profile.AwemeCount != 42 {
		t.Fatalf("profile: %v %+v", err, profile)
	}

	page, err := provider.PostsPage(ctx, "secQ", "", 20)
	if err != nil {
		t.Fatalf("posts: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Duration != 45000 || page.Items[0].MixID != "" {
		t.Fatalf("posts decoded wrong: %+v", page.Items)
	}
	if !page.HasMore || page.NextCursor == nil || *page.NextCursor != "120" {
		t.Fatalf("posts paging wrong: has_more=%v next=%v", page.HasMore, page.NextCursor)
	}

	detail, err := provider.WorkDetail(ctx, "v1")
	if err != nil || len(detail.Variants) != 1 || detail.Variants[0].Quality != "1080p" {
		t.Fatalf("work: %v %+v", err, detail)
	}

	if ep.ensured == 0 {
		t.Fatal("Ensure must be called per business request")
	}
}

// An invalid cursor must be rejected before any sidecar call.
func TestSidecarInvalidCursor(t *testing.T) {
	provider, _ := newStubSidecar(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("sidecar must not be called with an invalid cursor")
	})
	if _, err := provider.PostsPage(context.Background(), "secX", "not-a-number", 20); err == nil {
		t.Fatal("invalid cursor must error")
	}
}

// Ensure the request context is honored: a canceled ctx aborts the call.
func TestSidecarContextCanceled(t *testing.T) {
	provider, _ := newStubSidecar(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"has_more":false,"next_cursor":null}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.PostsPage(ctx, "secX", "", 20); err == nil {
		t.Fatal("canceled context must abort the sidecar call")
	}
}

// Compile-time guard for the JSON shape we decode.
func TestSidecarDecodeNullMix(t *testing.T) {
	var page PostsPage
	raw := []byte(`{"items":[{"item_id":"a","mix_id":null,"published_at":null}],"has_more":false,"next_cursor":null}`)
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
