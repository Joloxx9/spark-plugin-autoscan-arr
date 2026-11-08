package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/httpclient"
)

// maxRecordsPerPoll bounds one poll. A longer backlog (after a long outage)
// is worked through over several polls: the marker stops at the last record
// handled.
const maxRecordsPerPoll = 2000

// markerPrefix versions the marker format, so a marker this version cannot
// read is recognized and resynchronized instead of misread.
const markerPrefix = "v1|"

// app is the kind of *arr server behind a connection.
type app int

const (
	appSonarr app = iota + 1
	appRadarr
)

// eventTypes are the history events that change files on disk, per app.
// Grabs, failed downloads and ignored downloads touch no library file.
func (a app) eventTypes() []string {
	if a == appSonarr {
		return []string{"downloadFolderImported", "seriesFolderImported", "episodeFileRenamed", "episodeFileDeleted"}
	}
	return []string{"downloadFolderImported", "movieFolderImported", "movieFileRenamed", "movieFileDeleted"}
}

// includeParam asks the history endpoint to embed the series or movie, whose
// folder is the fallback when a record carries no file path.
func (a app) includeParam() string {
	if a == appSonarr {
		return "includeSeries"
	}
	return "includeMovie"
}

// arrAPI is the part of the *arr v3 API a poll uses.
type arrAPI interface {
	GetJSON(ctx context.Context, path string, dest any) error
}

type systemStatus struct {
	AppName string `json:"appName"`
}

type mediaFolder struct {
	Path string `json:"path"`
}

type historyRecord struct {
	ID          int64              `json:"id"`
	Date        time.Time          `json:"date"`
	EventType   string             `json:"eventType"`
	SourceTitle string             `json:"sourceTitle"`
	Data        map[string]*string `json:"data"`
	Series      *mediaFolder       `json:"series"`
	Movie       *mediaFolder       `json:"movie"`
}

type historyPage struct {
	Records []historyRecord `json:"records"`
}

// position is where a poll left off: the last record handled. Records are
// ordered by date, then id.
type position struct {
	date time.Time
	id   int64
}

func (p position) after(r historyRecord) bool {
	if !r.Date.Equal(p.date) {
		return r.Date.After(p.date)
	}
	return r.ID > p.id
}

func (p position) marker() string {
	return markerPrefix + p.date.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(p.id, 10)
}

func parseMarker(marker string) (position, bool) {
	rest, ok := strings.CutPrefix(marker, markerPrefix)
	if !ok {
		return position{}, false
	}
	dateText, idText, ok := strings.Cut(rest, "|")
	if !ok {
		return position{}, false
	}
	date, err := time.Parse(time.RFC3339Nano, dateText)
	if err != nil {
		return position{}, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 0 {
		return position{}, false
	}
	return position{date: date, id: id}, true
}

// poll answers one PollChanges: the changes after marker and the new marker.
func poll(ctx context.Context, api arrAPI, marker string, now func() time.Time) (*pluginv1.PollChangesResponse, error) {
	kind, err := detectApp(ctx, api)
	if err != nil {
		return nil, err
	}
	from, ok := parseMarker(marker)
	if !ok {
		// First poll, or a marker this version did not write: start from the
		// newest record instead of replaying the whole history.
		latest, err := latestPosition(ctx, api, now)
		if err != nil {
			return nil, err
		}
		return &pluginv1.PollChangesResponse{NextMarker: latest.marker()}, nil
	}

	records, err := recordsSince(ctx, api, kind, from)
	if err != nil {
		return nil, err
	}
	if len(records) > maxRecordsPerPoll {
		records = records[:maxRecordsPerPoll]
	}
	next := from
	var changes []*pluginv1.ScanSourceChange
	seen := map[string]bool{}
	for _, record := range records {
		for _, change := range changesFor(record) {
			key := change.GetScope().String() + "\x00" + change.GetSourcePath()
			if !seen[key] {
				seen[key] = true
				changes = append(changes, change)
			}
		}
		next = position{date: record.Date, id: record.ID}
	}
	return &pluginv1.PollChangesResponse{NextMarker: next.marker(), Changes: changes}, nil
}

func detectApp(ctx context.Context, api arrAPI) (app, error) {
	var status systemStatus
	if err := api.GetJSON(ctx, "/api/v3/system/status", &status); err != nil {
		return 0, operatorError("could not read the server's status", err)
	}
	switch strings.ToLower(strings.TrimSpace(status.AppName)) {
	case "sonarr":
		return appSonarr, nil
	case "radarr":
		return appRadarr, nil
	}
	return 0, fmt.Errorf("the connection points at %q, not Sonarr or Radarr", status.AppName)
}

// latestPosition is the newest history record, or now when the history is
// empty.
func latestPosition(ctx context.Context, api arrAPI, now func() time.Time) (position, error) {
	var page historyPage
	if err := api.GetJSON(ctx, "/api/v3/history?page=1&pageSize=1&sortKey=date&sortDirection=descending", &page); err != nil {
		return position{}, operatorError("could not read the history", err)
	}
	if len(page.Records) == 0 {
		return position{date: now().UTC()}, nil
	}
	return position{date: page.Records[0].Date, id: page.Records[0].ID}, nil
}

// recordsSince returns the file-changing records after from, oldest first.
func recordsSince(ctx context.Context, api arrAPI, kind app, from position) ([]historyRecord, error) {
	var out []historyRecord
	for _, eventType := range kind.eventTypes() {
		query := url.Values{}
		query.Set("date", from.date.UTC().Format(time.RFC3339Nano))
		query.Set("eventType", eventType)
		query.Set(kind.includeParam(), "true")
		var records []historyRecord
		if err := api.GetJSON(ctx, "/api/v3/history/since?"+query.Encode(), &records); err != nil {
			return nil, operatorError("could not read the history", err)
		}
		for _, record := range records {
			if from.after(record) {
				out = append(out, record)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// changesFor turns one history record into the paths Silo should scan.
func changesFor(r historyRecord) []*pluginv1.ScanSourceChange {
	var out []*pluginv1.ScanSourceChange
	file := func(p string) {
		if isAbsolute(p) {
			out = append(out, &pluginv1.ScanSourceChange{SourcePath: p, Scope: pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_FILE})
		}
	}
	switch strings.ToLower(r.EventType) {
	case "downloadfolderimported", "seriesfolderimported", "moviefolderimported":
		file(r.data("importedPath"))
	case "episodefilerenamed", "moviefilerenamed":
		// The new name is scanned; the old one is reported so its catalog
		// row is marked missing.
		file(r.data("path"))
		file(r.data("sourcePath"))
	case "episodefiledeleted", "moviefiledeleted":
		// Sonarr and Radarr record the deleted file's path as the title.
		file(r.SourceTitle)
	}
	if len(out) == 0 {
		// No usable file path: scan the series or movie folder instead.
		if folder := r.folder(); isAbsolute(folder) {
			out = append(out, &pluginv1.ScanSourceChange{SourcePath: folder, Scope: pluginv1.ScanSourceChangeScope_SCAN_SOURCE_CHANGE_SCOPE_SUBTREE})
		}
	}
	return out
}

// data reads a history data value. Key case differs between versions
// ("ImportedPath" or "importedPath"), so it is matched without case.
func (r historyRecord) data(key string) string {
	for k, v := range r.Data {
		if v != nil && strings.EqualFold(k, key) {
			return strings.TrimSpace(*v)
		}
	}
	return ""
}

func (r historyRecord) folder() string {
	switch {
	case r.Series != nil:
		return strings.TrimSpace(r.Series.Path)
	case r.Movie != nil:
		return strings.TrimSpace(r.Movie.Path)
	}
	return ""
}

var windowsPath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// isAbsolute accepts the absolute paths an *arr server may report: Unix,
// Windows drive and UNC. Silo's path rewrites convert the separators.
func isAbsolute(p string) bool {
	return path.IsAbs(p) || windowsPath.MatchString(p) || strings.HasPrefix(p, `\\`)
}

// operatorError words a request failure for the Autoscan activity log
// without the API key or the full URL.
func operatorError(what string, err error) error {
	var status *httpclient.StatusError
	if errors.As(err, &status) {
		switch status.StatusCode {
		case 401, 403:
			return fmt.Errorf("%s: the API key was refused (HTTP %d)", what, status.StatusCode)
		case 404:
			return fmt.Errorf("%s: the server has no v3 API at this URL (HTTP 404)", what)
		}
		return fmt.Errorf("%s: HTTP %d", what, status.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: the server did not answer in time", what)
	}
	return fmt.Errorf("%s: the server could not be reached", what)
}
