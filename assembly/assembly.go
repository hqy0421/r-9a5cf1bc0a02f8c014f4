// Package assembly implements asynchronous, version-aware assembly of
// multi-page orders.
//
// Scraping an order usually starts from a main page which lists a set of
// detail pages together with the version of each detail.  A later update of
// the main page may keep using some of the previous detail versions.  Detail
// pages are fetched independently and can arrive in any order, late, more
// than once, or not at all.
//
// Assembler accepts a Manifest for every version of the main page and Detail
// payloads for every fetched detail page.  A complete order is published only
// when every detail required by its manifest is available **at the exact
// version required by that manifest**.  Published details keep the order of
// the manifest's detail list, regardless of the order in which the responses
// arrived.
//
// Details are scoped by order ID, detail key and detail version:
//
//   - a response for an old version can only satisfy a manifest which asks
//     for that old version; it can never be used for a manifest which asks
//     for a newer version,
//   - a response is never used for another order, even when two orders
//     reference the same detail key and version,
//   - the same (order, key, version) response delivered again is ignored.
//
// Versioned detail payloads are retained per order, so a new manifest can
// reuse detail versions that were already fetched for an earlier manifest
// without fetching them again.  When a new manifest cannot be completed --
// because a detail is missing, a detail fetch failed, or the version was
// cancelled -- the previously published complete order remains available
// through Latest; incomplete or older versions are never published over it.
//
// Assembler is independent of Collector: it can be driven from Colly
// callbacks, from another goroutine, or not at all.  A Collector continues to
// work as usual when no Assembler is used.  Assembler methods are safe for
// concurrent use, and manifests and details may be submitted in any order from
// any number of goroutines.
//
// By default publish callbacks run synchronously on the goroutine which
// submitted the manifest or detail, after the internal locks have been
// released.  WithAsyncPublishing moves them to a dedicated goroutine; call Wait
// (or Close) after collection to drain the queue.
//
// Example:
//
//	a := assembly.NewAssembler(assembly.WithPublisher(func(order assembly.CompleteOrder) {
//		fmt.Println(order.OrderID, order.Version, len(order.Details))
//	}))
//
//	// From the main page callback:
//	err := a.SubmitManifest(assembly.Manifest{
//		OrderID: "A-100",
//		Version: "2",
//		Details: []assembly.DetailRef{
//			{Key: "items", Version: "3"},
//			{Key: "shipping", Version: "1"}, // reused version
//		},
//	})
//
//	// From each detail response callback:
//	accepted, err := a.SubmitDetail(assembly.Detail{
//		OrderID: "A-100",
//		Ref:     assembly.DetailRef{Key: "items", Version: "3"},
//		Body:    resp.Body,
//	})
//
//	// Later:
//	order, ok := a.Latest("A-100")
package assembly

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	// ErrEmptyOrderID is returned when an order ID is missing.
	ErrEmptyOrderID = errors.New("assembly: empty order ID")
	// ErrEmptyDetailKey is returned when a detail reference has neither a
	// key nor a URL to use as its key.
	ErrEmptyDetailKey = errors.New("assembly: empty detail key")
	// ErrManifestConflict is returned when a manifest is submitted again
	// with the same order ID and version but with a different detail list.
	ErrManifestConflict = errors.New("assembly: manifest already exists with different details")
)

// DetailRef identifies one required detail page and the version of that page
// which the manifest requires.
//
// Key is the stable identity of the detail (for example a detail ID or a URL
// path).  When Key is empty, URL is used as the key.  Version is matched
// verbatim by SubmitDetail.  URL is informational and is never used for
// matching, so redirects and rewritten links do not break assembly.
type DetailRef struct {
	Key     string
	Version string
	URL     string
}

func (r DetailRef) normalize() DetailRef {
	if r.Key == "" {
		r.Key = r.URL
	}
	return r
}

func (r DetailRef) identity() detailKey {
	return detailKey{key: r.Key, version: r.Version}
}

type detailKey struct {
	key     string
	version string
}

// Manifest is one version of an order main page.  Details must be listed in
// the order in which the order's details should be published.
//
// Version identifies this version of the main page.  It must be ordered (see
// NaturalVersionCompare and WithVersionComparator); a newer complete version
// supersedes an older one.  An empty Version is valid and compares lower than
// any non-empty version.
type Manifest struct {
	OrderID string
	Version string
	Details []DetailRef
}

func (m Manifest) normalize() (Manifest, error) {
	if m.OrderID == "" {
		return Manifest{}, ErrEmptyOrderID
	}
	out := Manifest{OrderID: m.OrderID, Version: m.Version}
	out.Details = make([]DetailRef, len(m.Details))
	for i, ref := range m.Details {
		ref = ref.normalize()
		if ref.Key == "" {
			return Manifest{}, fmt.Errorf("assembly: order %q detail %d: %w", m.OrderID, i, ErrEmptyDetailKey)
		}
		out.Details[i] = ref
	}
	return out, nil
}

func (m Manifest) requires(k detailKey) bool {
	for _, ref := range m.Details {
		if ref.identity() == k {
			return true
		}
	}
	return false
}

// manifestsEqual reports whether two manifests have the same order and the same
// ordered detail list.  Versions are compared by the caller, which has already
// established that the two versions are equivalent.
func manifestsEqual(a, b Manifest) bool {
	if a.OrderID != b.OrderID || len(a.Details) != len(b.Details) {
		return false
	}
	for i := range a.Details {
		if a.Details[i].Key != b.Details[i].Key ||
			a.Details[i].Version != b.Details[i].Version {
			return false
		}
	}
	return true
}

// Detail is a fetched detail page.
//
// OrderID scopes the response to one order, Ref identifies the detail key and
// version, Body carries the raw response body and Payload carries an
// optional parsed representation.  Body and Payload are preserved untouched
// in the assembled order; Body is copied when it is submitted.
type Detail struct {
	OrderID string
	Ref     DetailRef
	Body    []byte
	Payload interface{}
}

func (d Detail) normalize() (Detail, error) {
	if d.OrderID == "" {
		return Detail{}, ErrEmptyOrderID
	}
	ref := d.Ref.normalize()
	if ref.Key == "" {
		return Detail{}, fmt.Errorf("assembly: order %q: %w", d.OrderID, ErrEmptyDetailKey)
	}
	out := d
	out.Ref = ref
	out.Body = cloneBytes(d.Body)
	return out, nil
}

// CompleteDetail is one detail of a published complete order.  Ref is exactly
// the reference declared by the manifest, including its position.
type CompleteDetail struct {
	Ref     DetailRef
	Body    []byte
	Payload interface{}
}

// CompleteOrder is a fully assembled version of an order.  Details follow the
// manifest's list order.
type CompleteOrder struct {
	OrderID string
	Version string
	Details []CompleteDetail
}

// Publisher receives complete orders.  It is called once per complete version
// that becomes newer than the last published version, in ascending version
// order.  By default it runs synchronously on the submitting goroutine after
// the assembler's internal lock has been released; WithAsyncPublishing moves the
// call to the publishing goroutine.
//
// A publisher may read the assembler (for example with Latest), but it must not
// submit manifests or details to the same assembler and must not call Close or
// Wait: those calls block until publishing finishes and would deadlock.
type Publisher func(CompleteOrder)

// Option configures an Assembler.
type Option func(*Assembler)

// WithPublisher sets the callback invoked for every published complete order.
// Without a publisher, complete orders are still recorded and can be read with
// Latest.
func WithPublisher(p Publisher) Option {
	return func(a *Assembler) {
		if p != nil {
			a.publisher = p
		}
	}
}

// WithVersionComparator overrides the ordering used to decide whether a
// manifest version is newer than the currently published one.  The function
// must return a negative number when a is older than b, zero when they are
// the same version, and a positive number when a is newer than b.
func WithVersionComparator(compare func(a, b string) int) Option {
	return func(a *Assembler) {
		if compare != nil {
			a.compare = compare
		}
	}
}

// WithAsyncPublishing runs the publisher on a dedicated goroutine instead of
// on the goroutine that submitted the manifest or detail.  Call Wait after the
// collection has finished to block until every queued order has been
// published.
//
// By default the publisher runs synchronously on the submitting goroutine, after
// the assembler's locks have been released.  Both modes publish versions in the
// same order; with async publishing, a slow publisher no longer blocks the
// crawler callbacks.
func WithAsyncPublishing(async ...bool) Option {
	return func(a *Assembler) {
		if len(async) > 0 {
			a.async = async[0]
			return
		}
		a.async = true
	}
}

// Assembler assembles versioned multi-detail orders.  The zero value is not
// usable; create one with NewAssembler.  All methods are safe for concurrent
// use.
type Assembler struct {
	mu        sync.Mutex
	orders    map[string]*orderState
	publisher Publisher
	compare   func(a, b string) int

	async bool
	// queue carries complete orders to the publishing goroutine when async
	// publishing is enabled.  queueWG counts queued orders which have not
	// been delivered yet; loopWG tracks the publishing goroutine itself.
	queue     chan CompleteOrder
	queueWG   sync.WaitGroup
	loopWG    sync.WaitGroup
	closeOnce sync.Once
	closed    bool

	// dispatchMu serializes publisher calls and keeps them monotonic across
	// concurrent submissions, so a publisher never observes an older version
	// after a newer one.
	dispatchMu     sync.Mutex
	lastDispatched map[string]dispatchedVersion
}

type dispatchedVersion struct {
	version string
	has     bool
}

type orderState struct {
	// published is the newest complete version published so far.
	published         *CompleteOrder
	publishedManifest Manifest
	// pending maps a manifest version to the still incomplete manifest.
	pending map[string]Manifest
	// details caches fetched detail payloads by (key, version).
	details map[detailKey]storedDetail
}

type storedDetail struct {
	body    []byte
	payload interface{}
}

// NewAssembler creates an empty Assembler.
func NewAssembler(options ...Option) *Assembler {
	a := &Assembler{
		orders:         make(map[string]*orderState),
		compare:        NaturalVersionCompare,
		lastDispatched: make(map[string]dispatchedVersion),
	}
	for _, option := range options {
		if option != nil {
			option(a)
		}
	}
	if a.async {
		a.queue = make(chan CompleteOrder, 16)
		a.loopWG.Add(1)
		go a.publishLoop()
	}
	return a
}

// SubmitManifest registers a version of an order main page.
//
// Submitting an identical manifest for an already pending or already published
// version is a no-op.  A manifest older than the published version is ignored;
// it cannot replace a newer complete order.  If the manifest is already
// satisfiable from cached detail versions, a complete order is published
// immediately.
//
// An error is returned only for an invalid manifest or when the same version
// was previously submitted with a different detail list (ErrManifestConflict).
func (a *Assembler) SubmitManifest(manifest Manifest) error {
	normalized, err := manifest.normalize()
	if err != nil {
		return err
	}

	a.mu.Lock()
	state := a.state(normalized.OrderID)

	if state.published != nil {
		c := a.compare(normalized.Version, state.published.Version)
		switch {
		case c < 0:
			// A stale main page can never replace a newer complete order.
			a.mu.Unlock()
			return nil
		case c == 0:
			if !manifestsEqual(normalized, state.publishedManifest) {
				a.mu.Unlock()
				return fmt.Errorf("%w: order %q version %q", ErrManifestConflict, normalized.OrderID, normalized.Version)
			}
			a.mu.Unlock()
			return nil
		}
	}

	// An existing pending manifest whose version compares equal to the new
	// one is the same version expressed differently (for example "01" and
	// "1"); keep the first spelling and require the same detail list.
	if pending, ok := state.pendingVersion(normalized.Version, a.compare); ok {
		if !manifestsEqual(normalized, pending) {
			a.mu.Unlock()
			return fmt.Errorf("%w: order %q version %q", ErrManifestConflict, normalized.OrderID, normalized.Version)
		}
		a.mu.Unlock()
		return nil
	}

	state.pending[normalized.Version] = normalized
	published := a.collectComplete(state)
	a.mu.Unlock()

	a.dispatch(published)
	return nil
}

// SubmitDetail registers a fetched detail page.
//
// The response is matched by order ID, detail key and exact detail version.  A
// new response is cached even when no pending manifest currently requires it,
// so a later main page update can reuse that version.  A second response for
// the same (order, key, version) is ignored: accepted is false for a duplicate
// and true when the payload was stored for the first time.
//
// If storing the response completes one or more pending manifests, the
// complete orders are published before SubmitDetail returns.  Responses for
// old versions therefore only complete manifests which require those old
// versions; they never contribute to a newer manifest and never cause an
// older complete order to be published over a newer one.
func (a *Assembler) SubmitDetail(detail Detail) (accepted bool, err error) {
	normalized, err := detail.normalize()
	if err != nil {
		return false, err
	}

	a.mu.Lock()
	state := a.state(normalized.OrderID)
	key := normalized.Ref.identity()
	if _, exists := state.details[key]; exists {
		a.mu.Unlock()
		return false, nil
	}
	state.details[key] = storedDetail{
		body:    normalized.Body,
		payload: normalized.Payload,
	}
	published := a.collectComplete(state)
	a.mu.Unlock()

	a.dispatch(published)
	return true, nil
}

// FailDetail reports that fetching a detail version failed.  Every pending
// manifest which requires that exact version can no longer become complete and
// is discarded.  The previously published complete order is retained.  The
// discarded version may be retried by submitting its manifest again.
//
// FailDetail returns true when at least one pending manifest was discarded.
func (a *Assembler) FailDetail(orderID string, ref DetailRef) bool {
	if orderID == "" {
		return false
	}
	ref = ref.normalize()
	if ref.Key == "" {
		return false
	}
	key := ref.identity()

	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.orders[orderID]
	if !ok {
		return false
	}
	discarded := false
	for version, manifest := range state.pending {
		if manifest.requires(key) {
			delete(state.pending, version)
			discarded = true
		}
	}
	return discarded
}

// Cancel abandons a pending version of an order.  Cancelling a pending version
// keeps the previously published complete order.  A published version is never
// removed by Cancel.  Cancel returns true when a pending manifest was
// discarded.
func (a *Assembler) Cancel(orderID, version string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.orders[orderID]
	if !ok {
		return false
	}
	if _, ok := state.pending[version]; !ok {
		return false
	}
	delete(state.pending, version)
	return true
}

// FailManifest reports that a version of an order failed as a whole.  It
// discards the pending manifest and retains the previously published complete
// order.  It is equivalent to Cancel and returns true when a pending manifest
// was discarded.
func (a *Assembler) FailManifest(orderID, version string) bool {
	return a.Cancel(orderID, version)
}

// Latest returns the most recently published complete order for orderID.  An
// incomplete or failed newer version does not change this value.
func (a *Assembler) Latest(orderID string) (CompleteOrder, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.orders[orderID]
	if !ok || state.published == nil {
		return CompleteOrder{}, false
	}
	return cloneCompleteOrder(*state.published), true
}

// Pending returns the manifests of orderID that have not been published yet,
// ordered from oldest to newest version.  The returned manifests are copies.
func (a *Assembler) Pending(orderID string) []Manifest {
	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.orders[orderID]
	if !ok || len(state.pending) == 0 {
		return nil
	}

	versions := make([]string, 0, len(state.pending))
	for version := range state.pending {
		versions = append(versions, version)
	}
	compare := a.compare
	sort.Slice(versions, func(i, j int) bool {
		if c := compare(versions[i], versions[j]); c != 0 {
			return c < 0
		}
		return versions[i] < versions[j]
	})

	out := make([]Manifest, 0, len(versions))
	for _, version := range versions {
		out = append(out, cloneManifest(state.pending[version]))
	}
	return out
}

// Missing returns the details of manifest which are not cached yet, in
// manifest order.  A crawler can use it to fetch only the detail versions that
// a new main page actually needs while reusing all the others, and an empty
// result means the manifest is already complete (submitting it publishes the
// order immediately).
//
// Missing does not register the manifest; call SubmitManifest to do that.
func (a *Assembler) Missing(manifest Manifest) ([]DetailRef, error) {
	normalized, err := manifest.normalize()
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	state, ok := a.orders[normalized.OrderID]
	if !ok {
		return cloneRefs(normalized.Details), nil
	}
	missing := make([]DetailRef, 0, len(normalized.Details))
	for _, ref := range normalized.Details {
		if _, cached := state.details[ref.identity()]; !cached {
			missing = append(missing, ref)
		}
	}
	return missing, nil
}

// Forget drops all state for orderID, including cached details and the
// published complete order.  It returns true when the assembler held state for
// the order.  It is primarily useful in long-running processes to release
// memory once an order is no longer needed.
func (a *Assembler) Forget(orderID string) bool {
	a.mu.Lock()
	_, ok := a.orders[orderID]
	if ok {
		delete(a.orders, orderID)
	}
	a.mu.Unlock()

	if ok {
		a.dispatchMu.Lock()
		delete(a.lastDispatched, orderID)
		a.dispatchMu.Unlock()
	}
	return ok
}

func (a *Assembler) state(orderID string) *orderState {
	state, ok := a.orders[orderID]
	if !ok {
		state = &orderState{
			pending: make(map[string]Manifest),
			details: make(map[detailKey]storedDetail),
		}
		a.orders[orderID] = state
	}
	return state
}

// collectComplete publishes every pending manifest of state which is
// satisfiable from cached details and is newer than the currently published
// version.  It must be called with a.mu held.  The returned orders are
// dispatched by the caller after unlocking.
func (a *Assembler) collectComplete(state *orderState) []CompleteOrder {
	var completeVersions []string
	for version, manifest := range state.pending {
		if state.complete(manifest) {
			completeVersions = append(completeVersions, version)
		}
	}
	if len(completeVersions) == 0 {
		return nil
	}

	compare := a.compare
	sort.Slice(completeVersions, func(i, j int) bool {
		if c := compare(completeVersions[i], completeVersions[j]); c != 0 {
			return c < 0
		}
		return completeVersions[i] < completeVersions[j]
	})

	var published []CompleteOrder
	for _, version := range completeVersions {
		manifest := state.pending[version]
		if state.published != nil && compare(version, state.published.Version) <= 0 {
			// Never regress to an older (or duplicate) complete version.
			delete(state.pending, version)
			continue
		}
		order := state.build(manifest)
		state.published = &order
		state.publishedManifest = manifest
		delete(state.pending, version)
		published = append(published, cloneCompleteOrder(order))
	}

	// Pending manifests which are no longer newer than the published version
	// can never be published; release them.  Their cached detail versions are
	// retained so later manifests can still reuse them.
	if state.published != nil {
		for version := range state.pending {
			if compare(version, state.published.Version) <= 0 {
				delete(state.pending, version)
			}
		}
	}
	return published
}

// pendingVersion returns the pending manifest whose version compares equal to
// version, if any.
func (s *orderState) pendingVersion(version string, compare func(a, b string) int) (Manifest, bool) {
	if pending, ok := s.pending[version]; ok {
		return pending, true
	}
	for pendingVersion, pending := range s.pending {
		if compare(pendingVersion, version) == 0 {
			return pending, true
		}
	}
	return Manifest{}, false
}

func (s *orderState) complete(manifest Manifest) bool {
	for _, ref := range manifest.Details {
		if _, ok := s.details[ref.identity()]; !ok {
			return false
		}
	}
	return true
}

func (s *orderState) build(manifest Manifest) CompleteOrder {
	order := CompleteOrder{
		OrderID: manifest.OrderID,
		Version: manifest.Version,
		Details: make([]CompleteDetail, 0, len(manifest.Details)),
	}
	for _, ref := range manifest.Details {
		stored := s.details[ref.identity()]
		order.Details = append(order.Details, CompleteDetail{
			Ref:     ref,
			Body:    cloneBytes(stored.body),
			Payload: stored.payload,
		})
	}
	return order
}

func (a *Assembler) dispatch(orders []CompleteOrder) {
	if a.publisher == nil || len(orders) == 0 {
		return
	}

	// serialize publisher calls (or enqueueing) so that, when submissions
	// race, a publisher never observes an older version after a newer one.
	a.dispatchMu.Lock()
	defer a.dispatchMu.Unlock()
	if a.closed {
		return
	}
	for _, order := range orders {
		last := a.lastDispatched[order.OrderID]
		if last.has && a.compare(order.Version, last.version) <= 0 {
			continue
		}
		a.lastDispatched[order.OrderID] = dispatchedVersion{version: order.Version, has: true}
		if a.queue != nil {
			a.queueWG.Add(1)
			a.queue <- cloneCompleteOrder(order)
			continue
		}
		a.publisher(order)
	}
}

func (a *Assembler) publishLoop() {
	defer a.loopWG.Done()
	for order := range a.queue {
		a.publisher(order)
		a.queueWG.Done()
	}
}

// Wait blocks until every order queued by asynchronous publishing has been
// delivered to the publisher.  It is a no-op unless WithAsyncPublishing is
// used.  After Wait returns the assembler still accepts further submissions.
func (a *Assembler) Wait() {
	if a.queue == nil {
		return
	}
	// Holding dispatchMu guarantees no new order can be queued while waiting.
	a.dispatchMu.Lock()
	defer a.dispatchMu.Unlock()
	a.queueWG.Wait()
}

// Close stops the asynchronous publishing goroutine and waits for orders
// already queued to be published.  It is a no-op unless WithAsyncPublishing is
// used.  The assembler must not be used after Close.
func (a *Assembler) Close() {
	if a.queue == nil {
		return
	}
	a.closeOnce.Do(func() {
		a.dispatchMu.Lock()
		a.closed = true
		close(a.queue)
		a.dispatchMu.Unlock()
	})
	a.loopWG.Wait()
}

func cloneCompleteOrder(order CompleteOrder) CompleteOrder {
	out := order
	out.Details = make([]CompleteDetail, len(order.Details))
	for i, detail := range order.Details {
		detail.Body = cloneBytes(detail.Body)
		out.Details[i] = detail
	}
	return out
}

func cloneManifest(manifest Manifest) Manifest {
	out := manifest
	out.Details = cloneRefs(manifest.Details)
	return out
}

func cloneRefs(refs []DetailRef) []DetailRef {
	out := make([]DetailRef, len(refs))
	copy(out, refs)
	return out
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
