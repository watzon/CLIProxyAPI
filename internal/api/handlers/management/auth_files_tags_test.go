package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestBuildAuthFileEntryExposesCredentialTags(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	filePath := func(name string) map[string]string {
		path := filepath.Join(authDir, name)
		if errWrite := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
		return map[string]string{"path": path}
	}
	tagged := &coreauth.Auth{
		ID:         "tagged",
		FileName:   "tagged.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: filePath("tagged.json"),
		Metadata:   map[string]any{"tags": []any{"Work", "personal", "work"}},
	}
	entry := h.buildAuthFileEntry(tagged, authFileQuotaContext{v8: true})
	if got, want := entry["tags"], []string{"work", "personal"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entry tags = %#v, want %#v", got, want)
	}

	untagged := &coreauth.Auth{
		ID:         "untagged",
		FileName:   "untagged.json",
		Provider:   "claude",
		Status:     coreauth.StatusActive,
		Attributes: filePath("untagged.json"),
		Metadata:   map[string]any{},
	}
	if entry = h.buildAuthFileEntry(untagged, authFileQuotaContext{v8: true}); entry["tags"] != nil {
		t.Fatalf("untagged entry tags = %#v, want none", entry["tags"])
	}
}

func TestListAuthFilesFromDiskExposesCredentialTags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	authDir := t.TempDir()
	files := map[string]string{
		"tagged.json":   `{"type":"claude","email":"a@example.com","tags":["work"]}`,
		"string.json":   `{"type":"claude","email":"b@example.com","tags":"personal, clients"}`,
		"untagged.json": `{"type":"claude","email":"c@example.com"}`,
	}
	for name, body := range files {
		if errWrite := os.WriteFile(filepath.Join(authDir, name), []byte(body), 0o600); errWrite != nil {
			t.Fatal(errWrite)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v8/management/credentials", nil)
	h.ListAuthFiles(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Files []struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		} `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	want := map[string][]string{"tagged.json": {"work"}, "string.json": {"personal", "clients"}, "untagged.json": nil}
	if len(body.Files) != len(want) {
		t.Fatalf("files = %+v, want %d entries", body.Files, len(want))
	}
	for _, file := range body.Files {
		if !reflect.DeepEqual(file.Tags, want[file.Name]) {
			t.Fatalf("%s tags = %#v, want %#v", file.Name, file.Tags, want[file.Name])
		}
	}
}

func TestPatchAuthFileFieldsTags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name   string
		fields string
		status int
		want   []string
	}{
		{"set normalizes", `"tags":[" Work ","personal","work"]`, http.StatusOK, []string{"work", "personal"}},
		{"empty clears", `"tags":[]`, http.StatusOK, nil},
		{"whitespace clears", `"tags":[" "]`, http.StatusOK, nil},
		{"null clears", `"tags":null`, http.StatusOK, nil},
		{"string rejected", `"tags":"work"`, http.StatusBadRequest, []string{"old"}},
		{"number item rejected", `"tags":["work",1]`, http.StatusBadRequest, []string{"old"}},
		{"nested rejected", `"tags.extra":["work"]`, http.StatusBadRequest, []string{"old"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			record := &coreauth.Auth{
				ID: "credential.json", FileName: "credential.json", Provider: "claude",
				Metadata: map[string]any{"tags": []any{"old"}},
			}
			if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
				t.Fatal(errRegister)
			}
			h := NewHandlerWithoutConfigFilePath(&config.Config{}, manager)
			h.postAuthPersistHook = func(context.Context, *coreauth.Auth) error { return nil }

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPatch, "/v8/management/credentials/fields", strings.NewReader(`{"name":"credential.json",`+tt.fields+`}`))
			c.Request.Header.Set("Content-Type", "application/json")
			h.PatchAuthFileFields(c)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.status, rec.Body.String())
			}
			updated, ok := manager.GetByID(record.ID)
			if !ok {
				t.Fatal("auth missing")
			}
			if got := coreauth.CredentialTags(updated); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("tags = %#v, want %#v", got, tt.want)
			}
			if _, exists := updated.Metadata["tags"]; tt.want == nil && exists {
				t.Fatalf("cleared tags left metadata entry %#v", updated.Metadata["tags"])
			}
		})
	}
}
