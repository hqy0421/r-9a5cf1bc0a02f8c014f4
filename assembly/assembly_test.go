package assembly

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func manifest(order string, version string, refs ...DetailRef) Manifest {
	return Manifest{OrderID: order, Version: version, Details: refs}
}

func ref(key, version string) DetailRef {
	return DetailRef{Key: key, Version: version}
}

func detail(order, key, version, body string) Detail {
	return Detail{OrderID: order, Ref: ref(key, version), Body: []byte(body)}
}

func bodies(t *testing.T, order CompleteOrder) []string {
	t.Helper()
	out := make([]string, len(order.Details))
	for i, d := range order.Details {
		out[i] = string(d.Body)
	}
	return out
}

func assertBodies(t *testing.T, order CompleteOrder, want ...string) {
	t.Helper()
	got := bodies(t, order)
	if len(got) != len(want) {
		t.Fatalf("detail count = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("detail %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

func assertKeys(t *testing.T, order CompleteOrder, want ...string) {
	t.Helper()
	if len(order.Details) != len(want) {
		t.Fatalf("detail count = %d, want %d", len(order.Details), len(want))
	}
	for i := range want {
		if order.Details[i].Ref.Key != want[i] {
			t.Fatalf("detail %d key = %q, want %q", i, order.Details[i].Ref.Key, want[i])
		}
	}
}

func TestPublishesOnlyWhenAllRequiredDetailsArePresent(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) {
		published = append(published, order)
	}))

	if err := a.SubmitManifest(manifest("A", "1", ref("head", "1"), ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 {
		t.Fatalf("published %d orders before any detail arrived", len(published))
	}

	if _, err := a.SubmitDetail(detail("A", "head", "1", "head-1")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 0 {
		t.Fatalf("published with only some details: %d", len(published))
	}

	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d orders, want 1", len(published))
	}
	if published[0].OrderID != "A" || published[0].Version != "1" {
		t.Fatalf("published %+v", published[0])
	}
	assertBodies(t, published[0], "head-1", "items-1")

	latest, ok := a.Latest("A")
	if !ok {
		t.Fatal("Latest reported no published order")
	}
	assertBodies(t, latest, "head-1", "items-1")
}

func TestDetailArrivingBeforeManifestIsKept(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	if accepted, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil || !accepted {
		t.Fatalf("SubmitDetail = %v, %v", accepted, err)
	}
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d, want 1", len(published))
	}
	assertBodies(t, published[0], "items-1")
}

func TestDetailsFollowManifestOrderRegardlessOfArrival(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	refs := []DetailRef{ref("first", "1"), ref("second", "2"), ref("third", "3")}
	if err := a.SubmitManifest(manifest("A", "1", refs...)); err != nil {
		t.Fatal(err)
	}

	// Deliver in reverse order.
	for i := len(refs) - 1; i >= 0; i-- {
		if _, err := a.SubmitDetail(detail("A", refs[i].Key, refs[i].Version, refs[i].Key+"-body")); err != nil {
			t.Fatal(err)
		}
	}
	if len(published) != 1 {
		t.Fatalf("published %d, want 1", len(published))
	}
	assertKeys(t, published[0], "first", "second", "third")
	assertBodies(t, published[0], "first-body", "second-body", "third-body")
}

func TestNewManifestReusesPreviouslyFetchedDetailVersions(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	if err := a.SubmitManifest(manifest("A", "1", ref("head", "1"), ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "head", "1", "head-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatal("version 1 was not published")
	}

	// Version 2 keeps the old head version but needs a new items version.
	if err := a.SubmitManifest(manifest("A", "2", ref("head", "1"), ref("items", "2"))); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatal("version 2 published while its new detail version was missing")
	}

	// The reused detail is not fetched again: only items@2 is needed.
	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d versions, want 2", len(published))
	}
	assertBodies(t, published[1], "head-1", "items-2")
	if published[1].Version != "2" {
		t.Fatalf("published version %q, want 2", published[1].Version)
	}
}

func TestLateOldDetailCannotSatisfyNewerManifest(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	// Version 2 requires items@2.  Only the stale items@1 eventually arrives.
	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"))); err != nil {
		t.Fatal(err)
	}
	accepted, err := a.SubmitDetail(detail("A", "items", "1", "stale"))
	if err != nil {
		t.Fatal(err)
	}
	if !accepted {
		t.Fatal("first response for items@1 should be accepted into the version cache")
	}
	if len(published) != 0 {
		t.Fatalf("stale detail published an order: %+v", published)
	}
	if _, ok := a.Latest("A"); ok {
		t.Fatal("stale detail produced a complete order")
	}
}

func TestLateOldDetailDoesNotOverwriteNewerCompleteOrder(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatal("version 1 was not published")
	}

	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d, want 2", len(published))
	}

	// A stale detail arriving now is cached but changes nothing.
	accepted, err := a.SubmitDetail(detail("A", "items", "1", "items-1-again-slow"))
	if err != nil {
		t.Fatal(err)
	}
	if accepted {
		t.Fatal("re-delivered items@1 should be treated as a duplicate")
	}

	latest, ok := a.Latest("A")
	if !ok {
		t.Fatal("no published order")
	}
	if latest.Version != "2" {
		t.Fatalf("latest version = %q, want 2", latest.Version)
	}
	assertBodies(t, latest, "items-2")
	if len(published) != 2 {
		t.Fatalf("stale detail re-published an order: %d publications", len(published))
	}
}

func TestOldManifestCannotReplaceNewerCompleteOrder(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}

	// The old main page arrives late and is already satisfiable from cache.
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	if len(published) != 1 || published[0].Version != "2" {
		t.Fatalf("publications = %+v, want only version 2", published)
	}
	latest, _ := a.Latest("A")
	if latest.Version != "2" {
		t.Fatalf("latest version = %q, want 2", latest.Version)
	}
}

func TestDetailsNeverMixBetweenOrders(t *testing.T) {
	var published []CompleteOrder
	a := NewAssembler(WithPublisher(func(order CompleteOrder) { published = append(published, order) }))

	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitManifest(manifest("B", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}

	if _, err := a.SubmitDetail(detail("A", "items", "1", "A-items")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 {
		t.Fatalf("published %d, want 1 (only order A)", len(published))
	}
	if published[0].OrderID != "A" {
		t.Fatalf("published order %q, want A", published[0].OrderID)
	}

	if _, err := a.SubmitDetail(detail("B", "items", "1", "B-items")); err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("published %d, want 2", len(published))
	}
	if published[1].OrderID != "B" {
		t.Fatalf("published order %q, want B", published[1].OrderID)
	}
	assertBodies(t, published[1], "B-items")

	latestA, _ := a.Latest("A")
	assertBodies(t, latestA, "A-items")
}

func TestDuplicateDetailIsNotAddedTwice(t *testing.T) {
	a := NewAssembler()

	if accepted, err := a.SubmitDetail(detail("A", "items", "1", "first")); err != nil || !accepted {
		t.Fatalf("first SubmitDetail = %v, %v", accepted, err)
	}
	if accepted, err := a.SubmitDetail(detail("A", "items", "1", "second")); err != nil || accepted {
		t.Fatalf("duplicate SubmitDetail = %v, %v", accepted, err)
	}

	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	latest, ok := a.Latest("A")
	if !ok {
		t.Fatal("no order published")
	}
	if len(latest.Details) != 1 {
		t.Fatalf("detail count = %d, want 1", len(latest.Details))
	}
	assertBodies(t, latest, "first")
}

func TestDuplicateManifestIsIgnoredAndConflictingOneRejected(t *testing.T) {
	a := NewAssembler()

	m := manifest("A", "1", ref("items", "1"))
	if err := a.SubmitManifest(m); err != nil {
		t.Fatal(err)
	}
	if err := a.SubmitManifest(m); err != nil {
		t.Fatalf("duplicate manifest rejected: %v", err)
	}

	err := a.SubmitManifest(manifest("A", "1", ref("items", "2")))
	if !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("conflicting manifest error = %v, want ErrManifestConflict", err)
	}
}

func TestMissingDetailKeepsPreviousCompleteOrder(t *testing.T) {
	a := NewAssembler()

	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	// Version 2 is missing a detail and must not disturb version 1.
	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"), ref("extra", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}

	latest, ok := a.Latest("A")
	if !ok {
		t.Fatal("previous complete order was lost")
	}
	if latest.Version != "1" {
		t.Fatalf("latest version = %q, want 1", latest.Version)
	}
	assertBodies(t, latest, "items-1")
}

func TestFailedDetailKeepsPreviousCompleteOrder(t *testing.T) {
	a := NewAssembler()

	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"), ref("extra", "1"))); err != nil {
		t.Fatal(err)
	}
	if !a.FailDetail("A", ref("extra", "1")) {
		t.Fatal("FailDetail did not discard the pending version")
	}

	// A detail arriving after the failure must not publish the failed version.
	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "extra", "1", "extra-1")); err != nil {
		t.Fatal(err)
	}

	latest, ok := a.Latest("A")
	if !ok || latest.Version != "1" {
		t.Fatalf("latest = %+v, %v; want version 1", latest, ok)
	}

	// Submitting the failed manifest again now completes it from the cache.
	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"), ref("extra", "1"))); err != nil {
		t.Fatal(err)
	}
	latest, _ = a.Latest("A")
	if latest.Version != "2" {
		t.Fatalf("latest version = %q after retry, want 2", latest.Version)
	}
}

func TestCancelledVersionKeepsPreviousCompleteOrder(t *testing.T) {
	a := NewAssembler()

	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"))); err != nil {
		t.Fatal(err)
	}
	if !a.Cancel("A", "2") {
		t.Fatal("Cancel reported nothing to cancel")
	}
	if a.Cancel("A", "2") {
		t.Fatal("Cancel reported a second cancellation")
	}

	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}
	latest, _ := a.Latest("A")
	if latest.Version != "1" {
		t.Fatalf("latest version = %q, want 1", latest.Version)
	}

	if !a.FailManifest("A", "1") == false {
		t.Fatal("FailManifest must not remove a published version")
	}
	latest, _ = a.Latest("A")
	if latest.Version != "1" {
		t.Fatal("published version was removed by FailManifest")
	}
}

func TestManifestFailureBeforeAnyPublishLeavesNoOrder(t *testing.T) {
	a := NewAssembler()
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if !a.FailManifest("A", "1") {
		t.Fatal("FailManifest did not discard pending version")
	}
	if _, ok := a.Latest("A"); ok {
		t.Fatal("failed first version produced an order")
	}
}

func TestConcurrentSubmissionsPublishMonotonicVersions(t *testing.T) {
	var mu sync.Mutex
	var got []string
	a := NewAssembler(WithPublisher(func(order CompleteOrder) {
		mu.Lock()
		got = append(got, order.Version)
		mu.Unlock()
	}))

	const n = 40
	// Manifest versions 1..n all require the same single detail version, so
	// they become complete together and must publish in ascending order.
	for i := 1; i <= n; i++ {
		if err := a.SubmitManifest(manifest("A", fmt.Sprint(i), ref("items", "1"))); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = a.SubmitDetail(detail("A", "items", "1", "items-1"))
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// Only the newest complete version supersedes the rest in one go, in
	// ascending order with no duplicates.
	if len(got) == 0 {
		t.Fatal("no version published")
	}
	for i := 1; i < len(got); i++ {
		if NaturalVersionCompare(got[i-1], got[i]) >= 0 {
			t.Fatalf("publications not strictly ascending: %v", got)
		}
	}
	latest, ok := a.Latest("A")
	if !ok || latest.Version != fmt.Sprint(n) {
		t.Fatalf("latest version = %q, want %d", latest.Version, n)
	}
}

func TestConcurrentDetailArrivalCompletesExactlyOnce(t *testing.T) {
	var mu sync.Mutex
	count := 0
	a := NewAssembler(WithPublisher(func(order CompleteOrder) {
		mu.Lock()
		count++
		mu.Unlock()
	}))

	refs := []DetailRef{ref("a", "1"), ref("b", "1"), ref("c", "1"), ref("d", "1")}
	if err := a.SubmitManifest(manifest("A", "1", refs...)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for _, r := range refs {
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(r DetailRef) {
				defer wg.Done()
				_, _ = a.SubmitDetail(detail("A", r.Key, r.Version, r.Key))
			}(r)
		}
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Fatalf("published %d times, want exactly 1", count)
	}
	latest, _ := a.Latest("A")
	assertKeys(t, latest, "a", "b", "c", "d")
}

func TestLatestAndPendingReturnCopies(t *testing.T) {
	a := NewAssembler()
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	latest, _ := a.Latest("A")
	latest.Details[0].Body[0] = 'X'
	latest.Details[0].Ref.Key = "mutated"

	again, _ := a.Latest("A")
	assertBodies(t, again, "items-1")
	if again.Details[0].Ref.Key != "items" {
		t.Fatalf("published order was mutated through Latest: %q", again.Details[0].Ref.Key)
	}

	if err := a.SubmitManifest(manifest("A", "2", ref("items", "2"))); err != nil {
		t.Fatal(err)
	}
	pending := a.Pending("A")
	if len(pending) != 1 {
		t.Fatalf("pending = %v", pending)
	}
	pending[0].Details[0].Key = "mutated"
	if a.Pending("A")[0].Details[0].Key != "items" {
		t.Fatal("pending manifest was mutated by caller")
	}
}

func TestURLIsUsedAsKeyWhenKeyIsMissing(t *testing.T) {
	a := NewAssembler()
	url := "https://example.test/orders/A/details/1"
	if err := a.SubmitManifest(Manifest{
		OrderID: "A",
		Version: "1",
		Details: []DetailRef{{Version: "9", URL: url}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(Detail{
		OrderID: "A",
		Ref:     DetailRef{Version: "9", URL: url},
		Body:    []byte("by-url"),
	}); err != nil {
		t.Fatal(err)
	}
	latest, ok := a.Latest("A")
	if !ok {
		t.Fatal("order with URL key not assembled")
	}
	assertBodies(t, latest, "by-url")
	if latest.Details[0].Ref.URL != url {
		t.Fatalf("URL not preserved: %q", latest.Details[0].Ref.URL)
	}
}

func TestPayloadIsPreserved(t *testing.T) {
	type parsed struct{ Text string }
	a := NewAssembler()
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(Detail{
		OrderID: "A",
		Ref:     ref("items", "1"),
		Body:    []byte("raw"),
		Payload: parsed{Text: "parsed"},
	}); err != nil {
		t.Fatal(err)
	}
	latest, _ := a.Latest("A")
	got, ok := latest.Details[0].Payload.(parsed)
	if !ok || got.Text != "parsed" {
		t.Fatalf("payload = %#v", latest.Details[0].Payload)
	}
}

func TestEquivalentVersionSpellingsAreOneVersion(t *testing.T) {
	a := NewAssembler()
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	// "01" compares equal to "1", so this is the same version, not a new one.
	if err := a.SubmitManifest(manifest("A", "01", ref("items", "1"))); err != nil {
		t.Fatalf("equivalent version spelling rejected: %v", err)
	}
	if pending := a.Pending("A"); len(pending) != 1 {
		t.Fatalf("pending = %v, want a single version", pending)
	}
	// A conflicting detail list for the same version is still a conflict.
	if err := a.SubmitManifest(manifest("A", "01", ref("items", "2"))); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("error = %v, want ErrManifestConflict", err)
	}
}

func TestValidation(t *testing.T) {
	a := NewAssembler()
	if err := a.SubmitManifest(Manifest{Version: "1"}); !errors.Is(err, ErrEmptyOrderID) {
		t.Fatalf("empty order ID manifest error = %v", err)
	}
	if err := a.SubmitManifest(Manifest{OrderID: "A", Version: "1", Details: []DetailRef{{Version: "1"}}}); !errors.Is(err, ErrEmptyDetailKey) {
		t.Fatalf("empty detail key error = %v", err)
	}
	if _, err := a.SubmitDetail(Detail{Ref: ref("items", "1")}); !errors.Is(err, ErrEmptyOrderID) {
		t.Fatalf("empty order ID detail error = %v", err)
	}
	if _, err := a.SubmitDetail(Detail{OrderID: "A"}); !errors.Is(err, ErrEmptyDetailKey) {
		t.Fatalf("empty detail key detail error = %v", err)
	}
}

func TestMissingReportsOnlyUncachedDetailVersions(t *testing.T) {
	a := NewAssembler()

	m := manifest("A", "2", ref("head", "1"), ref("items", "2"), ref("notes", "3"))
	missing, err := a.Missing(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 3 || missing[0].Key != "head" || missing[2].Key != "notes" {
		t.Fatalf("missing = %v, want all three refs in manifest order", missing)
	}

	if _, err := a.SubmitDetail(detail("A", "head", "1", "head-1")); err != nil {
		t.Fatal(err)
	}
	// A different version of the same key must not count as cached.
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	missing, err = a.Missing(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || missing[0].Key != "items" || missing[1].Key != "notes" {
		t.Fatalf("missing = %v, want items@2 and notes@3", missing)
	}

	if _, err := a.SubmitDetail(detail("A", "items", "2", "items-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "notes", "3", "notes-3")); err != nil {
		t.Fatal(err)
	}
	missing, err = a.Missing(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}

	// Missing does not register the manifest.
	if pending := a.Pending("A"); len(pending) != 0 {
		t.Fatalf("Missing registered manifest: %v", pending)
	}
	if _, ok := a.Latest("A"); ok {
		t.Fatal("Missing published an order")
	}
}

func TestAsyncPublishingDeliversAfterWait(t *testing.T) {
	var mu sync.Mutex
	var got []string
	release := make(chan struct{})
	a := NewAssembler(
		WithAsyncPublishing(),
		WithPublisher(func(order CompleteOrder) {
			<-release // a slow publisher must not block submissions
			mu.Lock()
			got = append(got, order.Version)
			mu.Unlock()
		}),
	)
	defer a.Close()

	for i := 1; i <= 3; i++ {
		if err := a.SubmitManifest(manifest("A", fmt.Sprint(i), ref("items", "1"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}

	close(release)
	a.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("published versions = %v, want all three in order", got)
	}
	for i := 1; i < len(got); i++ {
		if NaturalVersionCompare(got[i-1], got[i]) >= 0 {
			t.Fatalf("async publications not ascending: %v", got)
		}
	}
	latest, _ := a.Latest("A")
	if latest.Version != "3" {
		t.Fatalf("latest version = %q, want 3", latest.Version)
	}
}

func TestAsyncPublishingCloseIsIdempotent(t *testing.T) {
	done := make(chan struct{})
	a := NewAssembler(
		WithAsyncPublishing(),
		WithPublisher(func(CompleteOrder) { close(done) }),
	)
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	a.Close()
	a.Close()
	select {
	case <-done:
	default:
		t.Fatal("Close returned before the queued order was published")
	}
}

func TestWaitAndCloseAreNoOpsWhenSynchronous(t *testing.T) {
	a := NewAssembler()
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	a.Close()
	if _, ok := a.Latest("A"); !ok {
		t.Fatal("synchronous order missing")
	}
}

func TestForget(t *testing.T) {
	a := NewAssembler()
	if err := a.SubmitManifest(manifest("A", "1", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	if !a.Forget("A") {
		t.Fatal("Forget returned false for a known order")
	}
	if _, ok := a.Latest("A"); ok {
		t.Fatal("order still published after Forget")
	}
	if a.Forget("A") {
		t.Fatal("Forget returned true for an unknown order")
	}
}

func TestNaturalVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1", "2", -1},
		{"2", "10", -1},
		{"10", "9", 1},
		{"1.9.3", "1.10.0", -1},
		{"01", "1", 0},
		{"v2", "V2", 1},
		{"", "1", -1},
		{"1", "1", 0},
		{"2024-05-01", "2024-06-01", -1},
	}
	for _, c := range cases {
		if got := NaturalVersionCompare(c.a, c.b); got != c.want {
			t.Errorf("NaturalVersionCompare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := NaturalVersionCompare(c.b, c.a); got != -c.want {
			t.Errorf("NaturalVersionCompare(%q, %q) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestCustomVersionComparator(t *testing.T) {
	// Force lexicographic ordering so that "10" sorts below "9".
	a := NewAssembler(WithVersionComparator(func(a, b string) int {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	}))

	if err := a.SubmitManifest(manifest("A", "9", ref("items", "1"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "1", "items-1")); err != nil {
		t.Fatal(err)
	}
	// Under lexicographic ordering "10" < "9", so it must not supersede.
	if err := a.SubmitManifest(manifest("A", "10", ref("items", "10"))); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SubmitDetail(detail("A", "items", "10", "items-10")); err != nil {
		t.Fatal(err)
	}
	latest, _ := a.Latest("A")
	if latest.Version != "9" {
		t.Fatalf("latest version = %q, want 9", latest.Version)
	}
}
