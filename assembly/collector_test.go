package assembly_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/assembly"
)

func assertBodies(t *testing.T, order assembly.CompleteOrder, want ...string) {
	t.Helper()
	if len(order.Details) != len(want) {
		t.Fatalf("detail count = %d, want %d", len(order.Details), len(want))
	}
	for i := range want {
		if got := string(order.Details[i].Body); got != want[i] {
			t.Fatalf("detail %d = %q, want %q", i, got, want[i])
		}
	}
}

// orderSite serves a main page listing detail links and the detail pages
// themselves.  It lets a test change the current manifest version and the
// current version of each detail independently, which models an order main
// page update that reuses some old detail versions.
type orderSite struct {
	mu       sync.Mutex
	version  int
	details  map[string]int // detail key -> current version
	failing  map[string]int // detail key -> version that fails
	requests map[string]int // detail key -> request count
}

func (s *orderSite) set(version int, details map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = version
	s.details = details
}

func (s *orderSite) fail(key string, version int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[key] = version
}

func (s *orderSite) currentVersion() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

func (s *orderSite) currentDetails() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.details))
	for k, v := range s.details {
		out[k] = v
	}
	return out
}

func (s *orderSite) isFailing(key string, version int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failing[key] == version
}

func (s *orderSite) requestCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[key]
}

func (s *orderSite) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		version := s.version
		details := make(map[string]int, len(s.details))
		for k, v := range s.details {
			details[k] = v
		}
		s.mu.Unlock()

		fmt.Fprintf(w, "<html><body><h1>order v%d</h1><ul>", version)
		// Deliberately emit detail links in reverse key order so the test
		// proves the manifest order (not the page order) drives output.
		keys := []string{"gamma", "beta", "alpha"}
		for _, key := range keys {
			if v, ok := details[key]; ok {
				fmt.Fprintf(w, `<li><a class="detail" data-key="%s" data-version="%d" href="/detail/%s?version=%d">%s</a></li>`,
					key, v, key, v, key)
			}
		}
		fmt.Fprint(w, "</ul></body></html>")
	})
	mux.HandleFunc("/detail/", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/detail/")
		version, err := strconv.Atoi(r.URL.Query().Get("version"))
		if err != nil {
			http.Error(w, "bad version", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.requests[key]++
		failing := s.failing[key]
		s.mu.Unlock()
		if failing == version {
			http.Error(w, "detail unavailable", http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, "body:%s:%d", key, version)
	})
	return mux
}

func TestCollectorIntegrationPublishesCompleteOrder(t *testing.T) {
	site := &orderSite{
		version:  1,
		details:  map[string]int{"alpha": 1, "beta": 1, "gamma": 1},
		failing:  map[string]int{},
		requests: map[string]int{},
	}
	server := httptest.NewServer(site.handler())
	defer server.Close()

	var mu sync.Mutex
	var published []assembly.CompleteOrder
	assembler := assembly.NewAssembler(assembly.WithPublisher(func(order assembly.CompleteOrder) {
		mu.Lock()
		published = append(published, order)
		mu.Unlock()
	}))

	// The crawler is an ordinary Collector: normal callbacks still fire, and
	// an Assembler is only fed from them.
	var linksMu sync.Mutex
	var links []string
	c := colly.NewCollector(colly.AllowURLRevisit())
	c.OnHTML("a.detail", func(e *colly.HTMLElement) {
		linksMu.Lock()
		links = append(links, e.Request.AbsoluteURL(e.Attr("href")))
		linksMu.Unlock()
	})
	c.OnResponse(func(r *colly.Response) {
		if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
			return
		}
		if _, err := assembler.SubmitDetail(assembly.Detail{
			OrderID: "A-1",
			Ref: assembly.DetailRef{
				Key:     strings.TrimPrefix(r.Request.URL.Path, "/detail/"),
				Version: r.Request.URL.Query().Get("version"),
				URL:     r.Request.URL.String(),
			},
			Body: r.Body,
		}); err != nil {
			t.Error(err)
		}
	})
	c.OnScraped(func(r *colly.Response) {
		if r.Request.URL.Path != "/" {
			return
		}
		// One manifest per main page version, requiring all three details in
		// a fixed order which differs from the page's link order.
		if err := assembler.SubmitManifest(assembly.Manifest{
			OrderID: "A-1",
			Version: strconv.Itoa(site.currentVersion()),
			Details: []assembly.DetailRef{
				{Key: "alpha", Version: "1"},
				{Key: "beta", Version: "1"},
				{Key: "gamma", Version: "1"},
			},
		}); err != nil {
			t.Error(err)
			return
		}
		linksMu.Lock()
		pending := append([]string(nil), links...)
		linksMu.Unlock()
		for _, link := range pending {
			if err := r.Request.Visit(link); err != nil {
				t.Error(err)
			}
		}
	})

	if err := c.Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("published %d complete orders, want 1", len(published))
	}
	latest, ok := assembler.Latest("A-1")
	if !ok {
		t.Fatal("no assembled order")
	}
	assertBodies(t, latest, "body:alpha:1", "body:beta:1", "body:gamma:1")
	if latest.Version != "1" {
		t.Fatalf("version = %q, want 1", latest.Version)
	}
}

// TestCollectorUpdateReusesDetailVersionsAndKeepsCompleteOrder covers the
// main-page update flow end to end: version 2 reuses some version 1 details,
// one new detail fails, and the published version 1 order is retained.
func TestCollectorUpdateReusesDetailVersionsAndKeepsCompleteOrder(t *testing.T) {
	site := &orderSite{
		version:  1,
		details:  map[string]int{"alpha": 1, "beta": 1, "gamma": 1},
		failing:  map[string]int{},
		requests: map[string]int{},
	}
	server := httptest.NewServer(site.handler())
	defer server.Close()

	assembler := assembly.NewAssembler()
	newCollector := func() *colly.Collector {
		var linksMu sync.Mutex
		var links []string
		c := colly.NewCollector(colly.AllowURLRevisit())
		c.OnHTML("a.detail", func(e *colly.HTMLElement) {
			linksMu.Lock()
			links = append(links, e.Request.AbsoluteURL(e.Attr("href")))
			linksMu.Unlock()
		})
		c.OnResponse(func(r *colly.Response) {
			if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
				return
			}
			if _, err := assembler.SubmitDetail(assembly.Detail{
				OrderID: "A-1",
				Ref: assembly.DetailRef{
					Key:     strings.TrimPrefix(r.Request.URL.Path, "/detail/"),
					Version: r.Request.URL.Query().Get("version"),
					URL:     r.Request.URL.String(),
				},
				Body: r.Body,
			}); err != nil {
				t.Error(err)
			}
		})
		c.OnError(func(r *colly.Response, err error) {
			if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
				return
			}
			assembler.FailDetail("A-1", assembly.DetailRef{
				Key:     strings.TrimPrefix(r.Request.URL.Path, "/detail/"),
				Version: r.Request.URL.Query().Get("version"),
			})
		})
		c.OnScraped(func(r *colly.Response) {
			if r.Request.URL.Path != "/" {
				return
			}
			if err := assembler.SubmitManifest(assembly.Manifest{
				OrderID: "A-1",
				Version: strconv.Itoa(site.currentVersion()),
				Details: []assembly.DetailRef{
					{Key: "alpha", Version: strconv.Itoa(site.currentDetails()["alpha"])},
					{Key: "beta", Version: strconv.Itoa(site.currentDetails()["beta"])},
					{Key: "gamma", Version: strconv.Itoa(site.currentDetails()["gamma"])},
				},
			}); err != nil {
				t.Error(err)
				return
			}
			linksMu.Lock()
			pending := append([]string(nil), links...)
			linksMu.Unlock()
			for _, link := range pending {
				// A detail which the site is configured to fail reports its
				// error to OnError and back to Visit; that is the failure
				// this test exercises, so only unexpected errors fail.
				if err := r.Request.Visit(link); err != nil && !strings.Contains(err.Error(), "Internal Server Error") {
					t.Error(err)
				}
			}
		})
		return c
	}

	if err := newCollector().Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
	v1, ok := assembler.Latest("A-1")
	if !ok || v1.Version != "1" {
		t.Fatalf("version 1 not assembled: %+v, %v", v1, ok)
	}
	alphaRequestsAfterV1 := site.requestCount("alpha")

	// Version 2 reuses alpha@1, refreshes beta@2 and fails gamma@2.
	site.set(2, map[string]int{"alpha": 1, "beta": 2, "gamma": 2})
	site.fail("gamma", 2)
	if err := newCollector().Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}

	latest, ok := assembler.Latest("A-1")
	if !ok {
		t.Fatal("published version lost after failed update")
	}
	if latest.Version != "1" {
		t.Fatalf("latest version = %q, want 1", latest.Version)
	}
	assertBodies(t, latest, "body:alpha:1", "body:beta:1", "body:gamma:1")

	if got := site.requestCount("alpha"); got <= alphaRequestsAfterV1 {
		t.Fatalf("alpha was not re-fetched: requests %d -> %d", alphaRequestsAfterV1, got)
	}

	// Now gamma@2 becomes available.  Submitting the same manifest again
	// reuses the cached alpha@1 and beta@2 and completes version 2.
	site.fail("gamma", 0)
	site.set(2, map[string]int{"alpha": 1, "beta": 2, "gamma": 2})
	if err := newCollector().Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
	latest, _ = assembler.Latest("A-1")
	if latest.Version != "2" {
		t.Fatalf("latest version = %q, want 2", latest.Version)
	}
	assertBodies(t, latest, "body:alpha:1", "body:beta:2", "body:gamma:2")
}

// TestCollectorWithoutAssemblerIsUnchanged verifies that importing and using
// the assembly package does not alter ordinary collection.
func TestCollectorWithoutAssemblerIsUnchanged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><a class="detail" href="/one">one</a></body></html>`)
	}))
	defer server.Close()

	var links int
	c := colly.NewCollector()
	c.OnHTML("a.detail", func(e *colly.HTMLElement) { links++ })
	if err := c.Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("links = %d, want 1", links)
	}
}

// TestErrorCallbackMarksDetailFailed demonstrates wiring a failed detail fetch
// into FailDetail so that a partially collected version is never published.
func TestErrorCallbackMarksDetailFailed(t *testing.T) {
	site := &orderSite{
		version:  2,
		details:  map[string]int{"alpha": 1, "beta": 2},
		failing:  map[string]int{"beta": 2},
		requests: map[string]int{},
	}
	server := httptest.NewServer(site.handler())
	defer server.Close()

	assembler := assembly.NewAssembler()

	// Publish version 1 of the order first, using real collection.
	site.set(1, map[string]int{"alpha": 1, "beta": 1})
	c := colly.NewCollector(colly.AllowURLRevisit())
	c.OnHTML("a.detail", func(e *colly.HTMLElement) {
		ref := assembly.DetailRef{
			Key:     e.Attr("data-key"),
			Version: e.Attr("data-version"),
		}
		e.Request.Ctx.Put("ref:"+ref.Key, ref.Version)
		_ = e.Request.Visit(e.Attr("href"))
	})
	c.OnResponse(func(r *colly.Response) {
		if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
			return
		}
		key := strings.TrimPrefix(r.Request.URL.Path, "/detail/")
		if _, err := assembler.SubmitDetail(assembly.Detail{
			OrderID: "A-1",
			Ref:     assembly.DetailRef{Key: key, Version: r.Request.URL.Query().Get("version")},
			Body:    r.Body,
		}); err != nil {
			t.Error(err)
		}
	})
	// The manifest for version 1 requires both details in a fixed order.
	if err := assembler.SubmitManifest(assembly.Manifest{
		OrderID: "A-1",
		Version: "1",
		Details: []assembly.DetailRef{
			{Key: "alpha", Version: "1"},
			{Key: "beta", Version: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}
	v1, ok := assembler.Latest("A-1")
	if !ok || v1.Version != "1" {
		t.Fatalf("version 1 not assembled: %+v", v1)
	}

	// Now version 2 reuses alpha@1 and needs beta@2, but beta@2 fails.
	site.set(2, map[string]int{"alpha": 1, "beta": 2})
	c2 := colly.NewCollector(colly.AllowURLRevisit())
	var failed []string
	var failMu sync.Mutex
	c2.OnHTML("a.detail", func(e *colly.HTMLElement) {
		_ = e.Request.Visit(e.Attr("href"))
	})
	c2.OnResponse(func(r *colly.Response) {
		if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
			return
		}
		key := strings.TrimPrefix(r.Request.URL.Path, "/detail/")
		_, _ = assembler.SubmitDetail(assembly.Detail{
			OrderID: "A-1",
			Ref:     assembly.DetailRef{Key: key, Version: r.Request.URL.Query().Get("version")},
			Body:    r.Body,
		})
	})
	c2.OnError(func(r *colly.Response, err error) {
		if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
			return
		}
		key := strings.TrimPrefix(r.Request.URL.Path, "/detail/")
		version := r.Request.URL.Query().Get("version")
		failMu.Lock()
		failed = append(failed, key+"@"+version)
		failMu.Unlock()
		if assembler.FailDetail("A-1", assembly.DetailRef{Key: key, Version: version}) {
			t.Log("discarded the pending incomplete version")
		}
	})
	if err := assembler.SubmitManifest(assembly.Manifest{
		OrderID: "A-1",
		Version: "2",
		Details: []assembly.DetailRef{
			{Key: "alpha", Version: "1"},
			{Key: "beta", Version: "2"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c2.Visit(server.URL + "/"); err != nil {
		t.Fatal(err)
	}

	failMu.Lock()
	gotFailed := append([]string(nil), failed...)
	failMu.Unlock()
	if len(gotFailed) == 0 {
		t.Fatal("expected the failed detail fetch to reach OnError")
	}

	latest, ok := assembler.Latest("A-1")
	if !ok {
		t.Fatal("previous complete version was lost")
	}
	if latest.Version != "1" {
		t.Fatalf("latest version = %q, want 1 after failed update", latest.Version)
	}
}
