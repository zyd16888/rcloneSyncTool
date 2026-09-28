package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDrivePublishUsesOneDirectoryMetadataOperation(t *testing.T) {
	f := newExecutionFixture(t, true)
	stage := "/.rclone-sync-staging/film/test"
	_ = os.MkdirAll(filepath.Join(f.remote, filepath.FromSlash(stage)), 0o755)
	_ = os.MkdirAll(filepath.Join(f.remote, "library"), 0o755)
	config := map[string]map[string]string{"mock": {"type": "drive", "token": `{"access_token":"fixture-token"}`}}
	encoded, _ := json.Marshal(config)
	_ = os.WriteFile(filepath.Join(f.remote, "mock-config.json"), encoded, 0o600)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPatch {
			t.Errorf("publication used %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("missing publish authorization")
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "Movie" {
			t.Errorf("wrong folder name: %+v", body)
		}
		if r.URL.Query().Get("addParents") == "" || r.URL.Query().Get("removeParents") == "" {
			t.Error("directory parent update missing")
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "mock-dir", "name": "Movie", "parents": []string{"mock-dir"}})
	}))
	defer server.Close()
	previous := driveAPIBase
	driveAPIBase = server.URL
	t.Cleanup(func() { driveAPIBase = previous })
	rule := f.rule
	rule.DstPath = "/library/Movie"
	settings, _ := f.st.RuntimeSettings(context.Background())
	if err := publishDirectory(context.Background(), rule, settings, stage); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("directory publication took %d requests", requests)
	}
}
