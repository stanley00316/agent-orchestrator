package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelRoutingSpawnFlagReachesDaemon(t *testing.T) {
	cfg := setConfigEnv(t)
	var got spawnRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case "/api/v1/projects/demo":
			_, _ = io.WriteString(w, `{"status":"ok","project":{"id":"demo","path":"/repo/demo","agent":"claude-code"}}`)
		case "/api/v1/sessions":
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, `{"session":{"id":"demo-1","status":"idle"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeRunFileFor(t, cfg, srv)
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "spawn", "--project", "demo", "--name", "routing test", "--routing-level", "2")
	if err != nil || got.RoutingLevel != 2 || got.Model != "" {
		t.Fatalf("request %+v, error %v", got, err)
	}
}
