// This example demonstrates assembling versioned order detail pages with
// Colly.
//
// A main page lists an order's detail pages together with the version of each
// detail.  When the main page is updated, some details keep their old version.
// Every detail response is submitted to an assembly.Assembler, which publishes
// a complete order only when every detail required by the newest main page is
// available at the exact version required, in the page's detail order.
package main

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/assembly"
)

// In this example the "site" is served from fixed strings.  The main page uses
// a reused (old) version of "shipping" and a new version of "items".
const mainPage = `<html><body>
<h1>Order A-100</h1>
<ul>
  <li><a class="detail" data-key="items" data-version="4" href="/detail/items/4">items</a></li>
  <li><a class="detail" data-key="shipping" data-version="2" href="/detail/shipping/2">shipping</a></li>
  <li><a class="detail" data-key="invoice" data-version="7" href="/detail/invoice/7">invoice</a></li>
</ul>
</body></html>`

var detailBodies = map[string]string{
	"/detail/items/4":    "items v4",
	"/detail/shipping/2": "shipping v2 (reused from the previous main page)",
	"/detail/invoice/7":  "invoice v7",
}

// detailRefs is the per-request accumulator for the manifest.  It is stored in
// the Request context so the detail pages visited from the main page inherit
// the same list.
type detailRefs []assembly.DetailRef

func main() {
	var mu sync.Mutex
	completed := make(map[string]assembly.CompleteOrder)

	a := assembly.NewAssembler(assembly.WithPublisher(func(order assembly.CompleteOrder) {
		mu.Lock()
		completed[order.OrderID] = order
		mu.Unlock()
		fmt.Printf("published order %s version %s with %d details\n",
			order.OrderID, order.Version, len(order.Details))
		for i, detail := range order.Details {
			fmt.Printf("  %d. %s@%s: %s\n", i+1, detail.Ref.Key, detail.Ref.Version, detail.Body)
		}
	}))

	c := colly.NewCollector()
	c.OnRequest(func(r *colly.Request) {
		if r.Ctx.GetAny("manifest") == nil {
			r.Ctx.Put("manifest", new(detailRefs))
		}
	})

	// The main page is the manifest: it lists which details, at which
	// versions, make a complete order.  Append in page order.
	c.OnHTML("a.detail", func(e *colly.HTMLElement) {
		refs, _ := e.Request.Ctx.GetAny("manifest").(*detailRefs)
		if refs == nil {
			return
		}
		*refs = append(*refs, assembly.DetailRef{
			Key:     e.Attr("data-key"),
			Version: e.Attr("data-version"),
			URL:     e.Request.AbsoluteURL(e.Attr("href")),
		})
	})

	c.OnResponse(func(r *colly.Response) {
		if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
			return
		}
		// Detail responses can arrive in any order; identity comes from the
		// key and version encoded in the URL.
		parts := strings.Split(strings.TrimPrefix(r.Request.URL.Path, "/detail/"), "/")
		if len(parts) != 2 {
			return
		}
		version, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}
		_, _ = a.SubmitDetail(assembly.Detail{
			OrderID: "A-100",
			Ref:     assembly.DetailRef{Key: parts[0], Version: strconv.Itoa(version)},
			Body:    r.Body,
		})
	})

	c.OnScraped(func(r *colly.Response) {
		if r.Request.URL.Path != "/" {
			return
		}
		// The main page finished parsing: register its manifest, then fetch
		// only the detail versions that are still missing.  Versions reused
		// from an earlier main page are already cached and are not fetched
		// again.
		refs, _ := r.Ctx.GetAny("manifest").(*detailRefs)
		manifest := assembly.Manifest{OrderID: "A-100", Version: "12", Details: *refs}
		if err := a.SubmitManifest(manifest); err != nil {
			log.Fatal(err)
		}
		missing, err := a.Missing(manifest)
		if err != nil {
			log.Fatal(err)
		}
		for _, ref := range missing {
			if err := r.Request.Visit(ref.URL); err != nil {
				log.Fatal(err)
			}
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		if !strings.HasPrefix(r.Request.URL.Path, "/detail/") {
			return
		}
		// A failed detail only discards this version; the previously
		// published complete order stays published.
		parts := strings.Split(strings.TrimPrefix(r.Request.URL.Path, "/detail/"), "/")
		if len(parts) == 2 {
			a.FailDetail("A-100", assembly.DetailRef{Key: parts[0], Version: parts[1]})
		}
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, mainPage)
			return
		}
		body, ok := detailBodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()

	if err := c.Visit(server.URL + "/"); err != nil {
		log.Fatal(err)
	}

	mu.Lock()
	order, ok := completed["A-100"]
	mu.Unlock()
	if !ok {
		log.Fatal("order was not assembled")
	}
	if len(order.Details) != 3 || order.Details[0].Ref.Key != "items" || order.Details[2].Ref.Key != "invoice" {
		log.Fatalf("unexpected assembled order: %+v", order)
	}
}
