// Command spark-plugin-autoscan-arr is a Silo Autoscan source for Sonarr and
// Radarr. Each poll reads the server's history since the previous poll and
// reports the files imported, renamed or deleted in between. The marker is the
// last history record handled; Silo stores it and sends it back, so the plugin
// keeps no state of its own.
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	goruntime "runtime"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/httpclient"
	publicmanifest "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// version is set at build time (-ldflags "-X main.version=...").
var version string

//go:embed manifest.json
var manifestJSON []byte

// requestTimeout bounds each call to Sonarr or Radarr; pollTimeout bounds the
// whole poll, well inside the five minutes Silo allows.
const (
	requestTimeout = time.Minute
	pollTimeout    = 3 * time.Minute
)

var httpClient = &http.Client{Timeout: requestTimeout}

type runtimeServer struct {
	pluginv1.UnimplementedRuntimeServer
	manifest *pluginv1.PluginManifest
}

func (s *runtimeServer) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: s.manifest}, nil
}

type scanSourceServer struct {
	pluginv1.UnimplementedScanSourceServer
}

func (scanSourceServer) PollChanges(ctx context.Context, req *pluginv1.PollChangesRequest) (*pluginv1.PollChangesResponse, error) {
	conn := req.GetConnection()
	baseURL := strings.TrimSpace(conn.GetBaseUrl())
	apiKey := strings.TrimSpace(conn.GetApiKey())
	if baseURL == "" || apiKey == "" {
		return nil, errors.New("choose a Sonarr or Radarr connection for this source")
	}
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	return poll(ctx, httpclient.New(baseURL, apiKey, httpClient), req.GetMarker(), time.Now)
}

func main() {
	manifest, err := loadManifest()
	if err != nil {
		panic(err)
	}
	sdkruntime.Serve(sdkruntime.ServeConfig{
		Servers: sdkruntime.CapabilityServers{
			Runtime:    &runtimeServer{manifest: manifest},
			ScanSource: scanSourceServer{},
		},
	})
}

// loadManifest reads the embedded manifest and stamps the binary's own
// checksum and version on it.
func loadManifest() (*pluginv1.PluginManifest, error) {
	manifest, err := publicmanifest.Load(manifestJSON)
	if err != nil {
		return nil, fmt.Errorf("load embedded manifest: %w", err)
	}
	if version != "" {
		manifest.Version = version
	}
	executablePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}
	binaryData, err := os.ReadFile(executablePath)
	if err != nil {
		return nil, fmt.Errorf("read executable %q: %w", executablePath, err)
	}
	checksum := sha256.Sum256(binaryData)
	manifest.Checksum = hex.EncodeToString(checksum[:])
	if len(manifest.GetSupportedPlatforms()) == 0 {
		manifest.SupportedPlatforms = []*pluginv1.SupportedPlatform{{Os: goruntime.GOOS, Arch: goruntime.GOARCH}}
	}
	return manifest, nil
}
