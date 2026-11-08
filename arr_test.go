package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/httpclient"
)

// fakeArr serves the v3 endpoints a poll reads, filtering history/since the
// way Sonarr and Radarr do: date on or after the given one, one event type.
type fakeArr struct {
	appName string
	history []map[string]any
	apiKey  string
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Api-Key") != f.apiKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/v3/system/status":
		_ = json.NewEncoder(w).Encode(map[string]any{"appName": f.appName})
	case "/api/v3/history":
		var records []map[string]any
		if n := len(f.history); n > 0 {
			records = append(records, f.history[n-1])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"records": records})
	case "/api/v3/history/since":
		since, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("date"))
		eventType := r.URL.Query().Get("eventType")
		out := []map[string]any{}
		for _, record := range f.history {
			date, _ := time.Parse(time.RFC3339Nano, record["date"].(string))
			if !date.Before(since) && record["eventType"] == eventType {
				out = append(out, record)
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func record(id int, date, eventType string, extra map[string]any) map[string]any {
	r := map[string]any{"id": id, "date": date, "eventType": eventType, "data": map[string]any{}}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func client(t *testing.T, f *fakeArr) *httpclient.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return httpclient.New(srv.URL, "key", srv.Client())
}

type change struct {
	path  string
	scope pluginv1.ScanSourceChangeScope
}

func changesOf(resp *pluginv1.PollChangesResponse) []change {
	var out []change
	for _, c := range resp.GetChanges() {
		out = append(out, change{c.GetSourcePath(), c.GetScope()})
	}
	return out
}

const (
	file    = pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE
	subtree = pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_SUBTREE
)

var fixedNow = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func TestFirstPollStartsAtTheNewestRecord(t *testing.T) {
	f := &fakeArr{appName: "Sonarr", apiKey: "key", history: []map[string]any{
		record(7, "2026-01-01T10:00:00Z", "downloadFolderImported", map[string]any{"data": map[string]any{"importedPath": "/tv/Show/S01E01.mkv"}}),
	}}
	resp, err := poll(context.Background(), client(t, f), "", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetChanges()) != 0 {
		t.Fatalf("first poll replayed history: %v", changesOf(resp))
	}
	if want := "v1|2026-01-01T10:00:00Z|7"; resp.GetNextMarker() != want {
		t.Fatalf("marker = %q, want %q", resp.GetNextMarker(), want)
	}

	empty := &fakeArr{appName: "Radarr", apiKey: "key"}
	resp, err = poll(context.Background(), client(t, empty), "not-a-marker", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1|2026-01-02T03:04:05Z|0"; resp.GetNextMarker() != want {
		t.Fatalf("empty history marker = %q, want %q", resp.GetNextMarker(), want)
	}
}

func TestSonarrEventsBecomeScans(t *testing.T) {
	series := map[string]any{"path": "/tv/Show"}
	f := &fakeArr{appName: "Sonarr", apiKey: "key", history: []map[string]any{
		record(1, "2026-01-01T10:00:00Z", "downloadFolderImported", map[string]any{"data": map[string]any{"ImportedPath": "/tv/Show/old.mkv"}}),
		record(2, "2026-01-01T11:00:00Z", "grabbed", map[string]any{"data": map[string]any{"importedPath": "/tv/ignored.mkv"}}),
		record(3, "2026-01-01T11:00:00Z", "downloadFolderImported", map[string]any{"data": map[string]any{"importedPath": "/tv/Show/S01E02.mkv"}, "series": series}),
		record(4, "2026-01-01T12:00:00Z", "episodeFileRenamed", map[string]any{"data": map[string]any{"sourcePath": "/tv/Show/a.mkv", "path": "/tv/Show/b.mkv"}}),
		record(5, "2026-01-01T13:00:00Z", "episodeFileDeleted", map[string]any{"sourceTitle": "/tv/Show/gone.mkv"}),
		record(6, "2026-01-01T14:00:00Z", "episodeFileDeleted", map[string]any{"sourceTitle": "gone.mkv", "series": series}),
		record(8, "2026-01-01T15:00:00Z", "downloadFolderImported", map[string]any{"data": map[string]any{"importedPath": "/tv/Show/S01E02.mkv"}}),
	}}
	// The marker sits on record 1, so it and the grab are not reported.
	resp, err := poll(context.Background(), client(t, f), "v1|2026-01-01T10:00:00Z|1", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []change{
		{"/tv/Show/S01E02.mkv", file},
		{"/tv/Show/b.mkv", file},
		{"/tv/Show/a.mkv", file},
		{"/tv/Show/gone.mkv", file},
		{"/tv/Show", subtree},
	}
	if got := changesOf(resp); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v\nwant %v", got, want)
	}
	if want := "v1|2026-01-01T15:00:00Z|8"; resp.GetNextMarker() != want {
		t.Fatalf("marker = %q, want %q", resp.GetNextMarker(), want)
	}

	// The same marker reads the same window again.
	again, err := poll(context.Background(), client(t, f), "v1|2026-01-01T10:00:00Z|1", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changesOf(again), want) {
		t.Fatalf("repeated poll differs: %v", changesOf(again))
	}
}

func TestRadarrEventsAndWindowsPaths(t *testing.T) {
	f := &fakeArr{appName: "Radarr", apiKey: "key", history: []map[string]any{
		record(10, "2026-01-01T10:00:00Z", "downloadFolderImported", map[string]any{"data": map[string]any{"importedPath": `D:\Movies\Film (2020)\Film.mkv`}}),
		record(11, "2026-01-01T10:00:00Z", "movieFileDeleted", map[string]any{"sourceTitle": "Film.mkv", "movie": map[string]any{"path": `\\nas\movies\Other (2021)`}}),
		record(12, "2026-01-01T11:00:00Z", "episodeFileDeleted", map[string]any{"sourceTitle": "/never/asked.mkv"}),
	}}
	resp, err := poll(context.Background(), client(t, f), "v1|2026-01-01T09:00:00Z|0", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []change{
		{`D:\Movies\Film (2020)\Film.mkv`, file},
		{`\\nas\movies\Other (2021)`, subtree},
	}
	if got := changesOf(resp); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %v\nwant %v", got, want)
	}
}

func TestNothingNewKeepsTheMarker(t *testing.T) {
	f := &fakeArr{appName: "Sonarr", apiKey: "key"}
	marker := "v1|2026-01-01T10:00:00Z|1"
	resp, err := poll(context.Background(), client(t, f), marker, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetNextMarker() != marker || len(resp.GetChanges()) != 0 {
		t.Fatalf("resp = %v", resp)
	}
}

func TestErrorsAreWrittenForTheOperator(t *testing.T) {
	f := &fakeArr{appName: "Sonarr", apiKey: "other"}
	_, err := poll(context.Background(), client(t, f), "", fixedNow)
	if err == nil || !strings.Contains(err.Error(), "API key was refused") {
		t.Fatalf("err = %v", err)
	}
	f = &fakeArr{appName: "Lidarr", apiKey: "key"}
	_, err = poll(context.Background(), client(t, f), "", fixedNow)
	if err == nil || !strings.Contains(err.Error(), "not Sonarr or Radarr") {
		t.Fatalf("err = %v", err)
	}
	_, err = scanSourceServer{}.PollChanges(context.Background(), &pluginv1.PollChangesRequest{})
	if err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("err = %v", err)
	}
}

func TestHistorySinceRequestsAreScoped(t *testing.T) {
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/system/status" {
			_, _ = w.Write([]byte(`{"appName":"Radarr"}`))
			return
		}
		queries = append(queries, r.URL.Query())
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if _, err := poll(context.Background(), httpclient.New(srv.URL, "key", srv.Client()), "v1|2026-01-01T10:00:00Z|1", fixedNow); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, q := range queries {
		if q.Get("includeMovie") != "true" || q.Get("date") != "2026-01-01T10:00:00Z" {
			t.Fatalf("query = %v", q)
		}
		types = append(types, q.Get("eventType"))
	}
	if want := appRadarr.eventTypes(); !reflect.DeepEqual(types, want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
}

func TestEmbeddedManifestLoads(t *testing.T) {
	m, err := loadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.GetPluginId() != "spark.autoscan.arr" || len(m.GetCapabilities()) != 1 {
		t.Fatalf("manifest = %v", m)
	}
}
