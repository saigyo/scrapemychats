package discover

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/saigyo/scrapemychats/internal/browser"
)

// ------------------------------------------------------------------ fakes

// scriptedResponse is one canned (status, body, err) reply.
type scriptedResponse struct {
	status int
	body   string
	err    error
}

// scriptedFetcher is a browser.Fetcher that replies from a fixed, ordered
// script and records every call it received, mirroring the fakeFetcher in
// internal/browser/fetch_test.go.
type scriptedFetcher struct {
	responses []scriptedResponse
	calls     []struct {
		url string
		hdr map[string]string
	}
}

func (f *scriptedFetcher) Fetch(apiURL string, headers map[string]string) (int, string, error) {
	f.calls = append(f.calls, struct {
		url string
		hdr map[string]string
	}{apiURL, headers})
	i := len(f.calls) - 1
	if i >= len(f.responses) {
		return 0, "", fmt.Errorf("scriptedFetcher: unexpected call #%d: %s", i, apiURL)
	}
	r := f.responses[i]
	return r.status, r.body, r.err
}

// stubSleep replaces the package sleep function for one test and returns
// the recorded durations, so pagination loops run instantly.
func stubSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	orig := sleep
	sleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleep = orig })
	return &slept
}

// genIDs returns n ids of the form prefixNNN, zero-padded to 3 digits.
func genIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	return ids
}

// itemsJSON builds a `{"items":[...],"total":N}` conversations-list body.
func itemsJSON(ids []string, total int) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q,"title":"Chat %s"}`, id, id)
	}
	fmt.Fprintf(&b, `],"total":%d}`, total)
	return b.String()
}

// -------------------------------------------------------------- CONV_ID_RE

func TestConvIDRE(t *testing.T) {
	tests := []struct {
		name  string
		url   string
		want  string
		match bool
	}{
		{"plain conversation", "https://chatgpt.com/c/12345678-1234-1234-1234-123456789012", "12345678-1234-1234-1234-123456789012", true},
		{"project conversation", "https://chatgpt.com/g/g-abc123/c/12345678-1234-1234-1234-123456789012", "12345678-1234-1234-1234-123456789012", true},
		{"no match", "https://chatgpt.com/library", "", false},
		{"malformed id too short", "https://chatgpt.com/c/1234-1234-1234-1234-123456789012", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ConvIDRE.FindStringSubmatch(tt.url)
			if tt.match {
				if m == nil {
					t.Fatalf("no match for %q", tt.url)
				}
				if m[1] != tt.want {
					t.Errorf("id = %q, want %q", m[1], tt.want)
				}
			} else if m != nil {
				t.Errorf("unexpected match %v for %q", m, tt.url)
			}
		})
	}
}

// -------------------------------------------------------------- DiscoverMain

func TestDiscoverMain_MultiPagePagination(t *testing.T) {
	slept := stubSleep(t)
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: itemsJSON(genIDs("p1-", 100), 250)},
		{status: 200, body: itemsJSON(genIDs("p2-", 100), 250)},
		{status: 200, body: itemsJSON(genIDs("p3-", 50), 250)},
	}}

	chats, err := DiscoverMain(f, map[string]string{"Authorization": "Bearer tok"})
	if err != nil {
		t.Fatalf("DiscoverMain: %v", err)
	}
	if len(chats) != 250 {
		t.Fatalf("got %d chats, want 250", len(chats))
	}
	if len(f.calls) != 3 {
		t.Fatalf("fetched %d times, want 3", len(f.calls))
	}
	wantURLs := []string{
		browser.BaseURL + "/backend-api/conversations?offset=0&limit=100&order=updated",
		browser.BaseURL + "/backend-api/conversations?offset=100&limit=100&order=updated",
		browser.BaseURL + "/backend-api/conversations?offset=200&limit=100&order=updated",
	}
	for i, want := range wantURLs {
		if f.calls[i].url != want {
			t.Errorf("call %d url = %q, want %q", i, f.calls[i].url, want)
		}
		if f.calls[i].hdr["Authorization"] != "Bearer tok" {
			t.Errorf("call %d did not forward headers: %v", i, f.calls[i].hdr)
		}
	}
	if len(*slept) != 3 {
		t.Errorf("slept %d times, want 3 (one after each non-empty page)", len(*slept))
	}
	if chats[0].ID != "p1-000" || chats[0].URL != browser.BaseURL+"/c/p1-000" {
		t.Errorf("first chat = %+v", chats[0])
	}
	if chats[249].ID != "p3-049" {
		t.Errorf("last chat = %+v", chats[249])
	}
}

func TestDiscoverMain_EmptyBatchTerminates(t *testing.T) {
	slept := stubSleep(t)
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: `{"items":[],"total":1000}`},
	}}

	chats, err := DiscoverMain(f, nil)
	if err != nil {
		t.Fatalf("DiscoverMain: %v", err)
	}
	if len(chats) != 0 {
		t.Errorf("got %d chats, want 0", len(chats))
	}
	if len(f.calls) != 1 {
		t.Errorf("fetched %d times, want 1 (empty batch must stop pagination)", len(f.calls))
	}
	if len(*slept) != 0 {
		t.Errorf("slept on an empty batch, want no sleep")
	}
}

// TestDiscoverMain_NullTotalKeepsPagingUntilEmptyBatch verifies the reviewer
// -confirmed Python behavior: `data.get("total", len(items))` only
// substitutes the len(items) default when the "total" key is ABSENT. An
// explicit `"total": null` yields None, and `while total is None or
// offset < total` keeps paging until an empty batch — it must not be
// treated as "total reached" after the first (or any) page.
func TestDiscoverMain_NullTotalKeepsPagingUntilEmptyBatch(t *testing.T) {
	slept := stubSleep(t)
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: `{"items":[{"id":"n1"},{"id":"n2"}],"total":null}`},
		{status: 200, body: `{"items":[{"id":"n3"},{"id":"n4"}],"total":null}`},
		{status: 200, body: `{"items":[],"total":null}`},
	}}

	chats, err := DiscoverMain(f, nil)
	if err != nil {
		t.Fatalf("DiscoverMain: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("fetched %d times, want 3 (a null total must not cut pagination short)", len(f.calls))
	}
	if len(chats) != 4 {
		t.Fatalf("got %d chats, want 4: %+v", len(chats), chats)
	}
	if len(*slept) != 2 {
		t.Errorf("slept %d times, want 2 (after each non-empty page)", len(*slept))
	}
}

func TestDiscoverMain_NumericIDIsKeptAndStringified(t *testing.T) {
	stubSleep(t)
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: `{"items":[{"id":123,"title":"Numeric"}],"total":1}`},
	}}

	chats, err := DiscoverMain(f, nil)
	if err != nil {
		t.Fatalf("DiscoverMain: %v", err)
	}
	if len(chats) != 1 {
		t.Fatalf("got %d chats, want 1: %+v", len(chats), chats)
	}
	want := Chat{URL: browser.BaseURL + "/c/123", ID: "123", Title: "Numeric"}
	if chats[0] != want {
		t.Errorf("chats[0] = %+v, want %+v", chats[0], want)
	}
}

func TestDiscoverMain_Non200ReturnsPythonErrorMessage(t *testing.T) {
	stubSleep(t)
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 500, body: "server error"},
	}}

	_, err := DiscoverMain(f, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	want := "conversation list request failed (HTTP 500). " +
		"You can build a chats.csv by hand instead — see README."
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestDiscoverMain_SkipsMissingIDAndFallsBackTitle(t *testing.T) {
	stubSleep(t)
	body := `{"items":[
		{"id":"has-id"},
		{"title":"no id here, must be dropped"},
		{"id":"titled","title":"Real Title"}
	],"total":3}`
	f := &scriptedFetcher{responses: []scriptedResponse{{status: 200, body: body}}}

	chats, err := DiscoverMain(f, nil)
	if err != nil {
		t.Fatalf("DiscoverMain: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("got %d chats, want 2: %+v", len(chats), chats)
	}
	if chats[0].ID != "has-id" || chats[0].Title != "untitled" {
		t.Errorf("chats[0] = %+v, want id=has-id title=untitled", chats[0])
	}
	if chats[1].ID != "titled" || chats[1].Title != "Real Title" {
		t.Errorf("chats[1] = %+v, want id=titled title=%q", chats[1], "Real Title")
	}
}

func TestDiscoverMain_FetchErrorPropagates(t *testing.T) {
	stubSleep(t)
	boom := errors.New("target closed")
	f := &scriptedFetcher{responses: []scriptedResponse{{err: boom}}}
	if _, err := DiscoverMain(f, nil); !errors.Is(err, boom) {
		t.Errorf("DiscoverMain error = %v, want %v", err, boom)
	}
}

// ---------------------------------------------------------- DiscoverProjects

func TestDiscoverProjects_SidebarNon200ReturnsEmptyNoError(t *testing.T) {
	stubSleep(t)
	f := &scriptedFetcher{responses: []scriptedResponse{{status: 404, body: "nope"}}}

	chats, err := DiscoverProjects(f, nil)
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if len(chats) != 0 {
		t.Errorf("got %d chats, want 0", len(chats))
	}
	if len(f.calls) != 1 {
		t.Errorf("fetched %d times, want 1 (must not fall through to project pages)", len(f.calls))
	}
}

// TestDiscoverProjects_SidebarPaginationAndGizmoShapes exercises the two
// gizmo shapes ("g = it.get('gizmo') or {}; g = g.get('gizmo') or g"), the
// sidebar cursor pagination, and the resulting /g/{gid}/c/{id} URL shape,
// all from a single realistic two-page sidebar listing.
func TestDiscoverProjects_SidebarPaginationAndGizmoShapes(t *testing.T) {
	slept := stubSleep(t)
	sidebarPage1 := `{"items":[
		{"gizmo":{"gizmo":{"id":"g1","display":{"name":"Proj One"}}}},
		{"gizmo":{"id":"g2","short_url":"proj-two"}},
		{"gizmo":{}}
	],"cursor":"CUR2"}`
	sidebarPage2 := `{"items":[{"gizmo":{"id":"g3"}}],"cursor":null}`

	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: sidebarPage1},
		{status: 200, body: sidebarPage2},
		// g1's conversations: one real chat, terminates on a null cursor.
		{status: 200, body: `{"items":[{"id":"c1","title":"Chat One"}],"cursor":null}`},
		// g2's conversations: empty, terminates on an empty batch.
		{status: 200, body: `{"items":[],"cursor":null}`},
		// g3's conversations: empty, terminates on an empty batch.
		{status: 200, body: `{"items":[],"cursor":null}`},
	}}

	chats, err := DiscoverProjects(f, nil)
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if len(f.calls) != 5 {
		t.Fatalf("fetched %d times, want 5: %+v", len(f.calls), f.calls)
	}

	// Sidebar pagination: first call has no ?cursor, second carries the
	// cursor returned by the first.
	if f.calls[0].url != browser.BaseURL+"/backend-api/gizmos/snorlax/sidebar" {
		t.Errorf("sidebar page1 url = %q", f.calls[0].url)
	}
	if f.calls[1].url != browser.BaseURL+"/backend-api/gizmos/snorlax/sidebar?cursor=CUR2" {
		t.Errorf("sidebar page2 url = %q", f.calls[1].url)
	}

	// Per-project conversation calls happen in discovery order (g1, g2,
	// g3) with the initial page_cursor=0.
	wantProjectURLs := []string{
		browser.BaseURL + "/backend-api/gizmos/g1/conversations?cursor=0&limit=50&owned_only=false",
		browser.BaseURL + "/backend-api/gizmos/g2/conversations?cursor=0&limit=50&owned_only=false",
		browser.BaseURL + "/backend-api/gizmos/g3/conversations?cursor=0&limit=50&owned_only=false",
	}
	for i, want := range wantProjectURLs {
		if got := f.calls[i+2].url; got != want {
			t.Errorf("project call %d url = %q, want %q", i, got, want)
		}
	}

	if len(chats) != 1 {
		t.Fatalf("got %d chats, want 1: %+v", len(chats), chats)
	}
	want := Chat{URL: browser.BaseURL + "/g/g1/c/c1", ID: "c1", Title: "Chat One"}
	if chats[0] != want {
		t.Errorf("chats[0] = %+v, want %+v", chats[0], want)
	}

	// One sleep between the two sidebar pages; none of the per-project
	// loops run a second page, so they contribute no sleeps.
	if len(*slept) != 1 {
		t.Errorf("slept %d times, want 1", len(*slept))
	}
}

// TestDiscoverProjects_CursorEqualityTermination checks the
// `nxt in (None, page_cursor)` termination: when the API returns the same
// cursor value twice in a row, pagination must stop rather than loop
// forever re-requesting the same page.
func TestDiscoverProjects_CursorEqualityTermination(t *testing.T) {
	slept := stubSleep(t)
	sidebar := `{"items":[{"gizmo":{"id":"gX","display":{"name":"X"}}}],"cursor":null}`
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: sidebar},
		{status: 200, body: `{"items":[{"id":"a"}],"cursor":"SAME"}`},
		{status: 200, body: `{"items":[{"id":"b"}],"cursor":"SAME"}`},
	}}

	chats, err := DiscoverProjects(f, nil)
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("fetched %d times, want 3 (must stop once cursor repeats)", len(f.calls))
	}
	if f.calls[1].url != browser.BaseURL+"/backend-api/gizmos/gX/conversations?cursor=0&limit=50&owned_only=false" {
		t.Errorf("page1 url = %q", f.calls[1].url)
	}
	if f.calls[2].url != browser.BaseURL+"/backend-api/gizmos/gX/conversations?cursor=SAME&limit=50&owned_only=false" {
		t.Errorf("page2 url = %q", f.calls[2].url)
	}
	if len(chats) != 2 || chats[0].ID != "a" || chats[1].ID != "b" {
		t.Fatalf("chats = %+v, want ids [a b]", chats)
	}
	// One sleep between page1 and page2; the repeated cursor on page2
	// must stop the loop before a third fetch or sleep.
	if len(*slept) != 1 {
		t.Errorf("slept %d times, want 1", len(*slept))
	}
}

// TestDiscoverProjects_NumericGidAndIDAreKept verifies Python's
// `if it.get("id")` truthiness check (and its f-string stringification)
// is replicated for numeric ids: a gizmo id of 456 (a bare JSON number,
// not a string) must still produce project "456", and a numeric
// conversation id of 789 must still produce a chat.
func TestDiscoverProjects_NumericGidAndIDAreKept(t *testing.T) {
	stubSleep(t)
	sidebar := `{"items":[{"gizmo":{"id":456,"display":{"name":"Numeric Project"}}}],"cursor":null}`
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: sidebar},
		{status: 200, body: `{"items":[{"id":789,"title":"Numeric Chat"}],"cursor":null}`},
	}}

	chats, err := DiscoverProjects(f, nil)
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if f.calls[1].url != browser.BaseURL+"/backend-api/gizmos/456/conversations?cursor=0&limit=50&owned_only=false" {
		t.Errorf("project conversations url = %q", f.calls[1].url)
	}
	if len(chats) != 1 {
		t.Fatalf("got %d chats, want 1: %+v", len(chats), chats)
	}
	want := Chat{URL: browser.BaseURL + "/g/456/c/789", ID: "789", Title: "Numeric Chat"}
	if chats[0] != want {
		t.Errorf("chats[0] = %+v, want %+v", chats[0], want)
	}
}

func TestDiscoverProjects_ProjectListingNon200SkipsProjectWithoutError(t *testing.T) {
	stubSleep(t)
	sidebar := `{"items":[{"gizmo":{"id":"gY","display":{"name":"Y"}}}],"cursor":null}`
	f := &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: sidebar},
		{status: 500, body: "boom"},
	}}

	chats, err := DiscoverProjects(f, nil)
	if err != nil {
		t.Fatalf("DiscoverProjects: %v", err)
	}
	if len(chats) != 0 {
		t.Errorf("got %d chats, want 0", len(chats))
	}
	if len(f.calls) != 2 {
		t.Errorf("fetched %d times, want 2 (must not retry the failed project page)", len(f.calls))
	}
}

func TestDiscoverProjects_SidebarFetchErrorPropagates(t *testing.T) {
	stubSleep(t)
	boom := errors.New("target closed")
	f := &scriptedFetcher{responses: []scriptedResponse{{err: boom}}}
	if _, err := DiscoverProjects(f, nil); !errors.Is(err, boom) {
		t.Errorf("DiscoverProjects error = %v, want %v", err, boom)
	}
}

// -------------------------------------------------------------- ReadChatList

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestReadChatList(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")
	content := "url,title\n" + // header row: doesn't start with http, skipped
		"https://chatgpt.com/c/11111111-1111-1111-1111-111111111111,First Chat\n" +
		"https://chatgpt.com/g/g-abc/c/22222222-2222-2222-2222-222222222222,Project Chat\n" +
		"https://chatgpt.com/c/33333333-3333-3333-3333-333333333333\n" + // ragged row, no title -> untitled
		"https://chatgpt.com/c/33333333-3333-3333-3333-333333333333,Duplicate\n" + // dup id, dropped
		"not a url,Ignored\n" + // no http prefix, skipped
		"https://chatgpt.com/no-conversation-id-here,Ignored Too\n" // http but no conv id, skipped+logged
	writeFile(t, csvPath, content)

	chats, err := ReadChatList(csvPath)
	if err != nil {
		t.Fatalf("ReadChatList: %v", err)
	}
	want := []Chat{
		{URL: "https://chatgpt.com/c/11111111-1111-1111-1111-111111111111", ID: "11111111-1111-1111-1111-111111111111", Title: "First Chat"},
		{URL: "https://chatgpt.com/g/g-abc/c/22222222-2222-2222-2222-222222222222", ID: "22222222-2222-2222-2222-222222222222", Title: "Project Chat"},
		{URL: "https://chatgpt.com/c/33333333-3333-3333-3333-333333333333", ID: "33333333-3333-3333-3333-333333333333", Title: "untitled"},
	}
	if len(chats) != len(want) {
		t.Fatalf("got %d chats, want %d: %+v", len(chats), len(want), chats)
	}
	for i := range want {
		if chats[i] != want[i] {
			t.Errorf("chats[%d] = %+v, want %+v", i, chats[i], want[i])
		}
	}
}

func TestReadChatList_BOM(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")
	bom := "\xEF\xBB\xBF"
	content := bom + "url,title\n" +
		"https://chatgpt.com/c/44444444-4444-4444-4444-444444444444,BOM Chat\n"
	writeFile(t, csvPath, content)

	chats, err := ReadChatList(csvPath)
	if err != nil {
		t.Fatalf("ReadChatList: %v", err)
	}
	if len(chats) != 1 || chats[0].ID != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("chats = %+v", chats)
	}
}

func TestReadChatList_MissingFile(t *testing.T) {
	if _, err := ReadChatList(filepath.Join(t.TempDir(), "missing.csv")); err == nil {
		t.Error("expected an error for a missing file")
	}
}

// -------------------------------------------------------------- DiscoverChats

// discoveryStub returns a scriptedFetcher wired to a minimal, deterministic
// DiscoverMain + DiscoverProjects run: one main-sidebar chat "m1" and no
// projects, in exactly two calls.
func discoveryStub() *scriptedFetcher {
	return &scriptedFetcher{responses: []scriptedResponse{
		{status: 200, body: `{"items":[{"id":"aaaa1111-1111-1111-1111-111111111111","title":"Main Chat"}],"total":1}`},
		{status: 200, body: `{"items":[],"cursor":null}`},
	}}
}

func TestDiscoverChats_FreshCSV(t *testing.T) {
	stubSleep(t)
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")

	if err := DiscoverChats(discoveryStub(), nil, csvPath); err != nil {
		t.Fatalf("DiscoverChats: %v", err)
	}

	got, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "url,title\nhttps://chatgpt.com/c/aaaa1111-1111-1111-1111-111111111111,Main Chat\n"
	if string(got) != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestDiscoverChats_MergesExistingPreservesOrderDedupsAppendsNew(t *testing.T) {
	stubSleep(t)
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")
	existing := "url,title\n" +
		"https://chatgpt.com/c/e1111111-1111-1111-1111-111111111111,Existing One\n" +
		"https://chatgpt.com/c/e2222222-2222-2222-2222-222222222222,Existing Two\n" +
		"not a url,Skip Me\n" // must be dropped, not preserved
	writeFile(t, csvPath, existing)

	if err := DiscoverChats(discoveryStub(), nil, csvPath); err != nil {
		t.Fatalf("DiscoverChats: %v", err)
	}

	got, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "url,title\n" +
		"https://chatgpt.com/c/e1111111-1111-1111-1111-111111111111,Existing One\n" +
		"https://chatgpt.com/c/e2222222-2222-2222-2222-222222222222,Existing Two\n" +
		"https://chatgpt.com/c/aaaa1111-1111-1111-1111-111111111111,Main Chat\n"
	if string(got) != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestDiscoverChats_DedupsAgainstExistingID(t *testing.T) {
	stubSleep(t)
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")
	// m1 is already present under a different title; the freshly
	// discovered m1 (title "Main Chat") must NOT be appended again, and
	// the existing title must be left untouched.
	existing := "url,title\nhttps://chatgpt.com/c/aaaa1111-1111-1111-1111-111111111111,Old Title\n"
	writeFile(t, csvPath, existing)

	if err := DiscoverChats(discoveryStub(), nil, csvPath); err != nil {
		t.Fatalf("DiscoverChats: %v", err)
	}

	got, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "url,title\nhttps://chatgpt.com/c/aaaa1111-1111-1111-1111-111111111111,Old Title\n"
	if string(got) != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestDiscoverChats_BOMInExistingFile(t *testing.T) {
	stubSleep(t)
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")
	bom := "\xEF\xBB\xBF"
	existing := bom + "url,title\nhttps://chatgpt.com/c/e1111111-1111-1111-1111-111111111111,Existing\n"
	writeFile(t, csvPath, existing)

	if err := DiscoverChats(discoveryStub(), nil, csvPath); err != nil {
		t.Fatalf("DiscoverChats: %v", err)
	}

	got, err := os.ReadFile(csvPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "url,title\n" +
		"https://chatgpt.com/c/e1111111-1111-1111-1111-111111111111,Existing\n" +
		"https://chatgpt.com/c/aaaa1111-1111-1111-1111-111111111111,Main Chat\n"
	if string(got) != want {
		t.Errorf("csv = %q, want %q", got, want)
	}
}

func TestDiscoverChats_DiscoverMainErrorPropagates(t *testing.T) {
	stubSleep(t)
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "chats.csv")
	f := &scriptedFetcher{responses: []scriptedResponse{{status: 500, body: "boom"}}}
	if err := DiscoverChats(f, nil, csvPath); err == nil {
		t.Error("expected an error")
	}
	if _, err := os.Stat(csvPath); err == nil {
		t.Error("csv file must not be written when discovery fails")
	}
}
