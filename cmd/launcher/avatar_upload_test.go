package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rf4server/internal/config"
	"rf4server/internal/store"

	"gopkg.in/yaml.v3"
)

func TestHandleAvatarUploadAndServe(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("# preserve this comment\ngateway:\n  advertise_host: 127.0.0.1\ngame:\n  obs_addr: ':9502'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "avatar.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	a := &app{
		cfg:        &config.Config{Gateway: config.Gateway{AdvertiseHost: "127.0.0.1"}},
		cfgPath:    configPath,
		avatarDir:  filepath.Join(dir, "avatars"),
		avatarPort: 8090,
		adminDB:    db,
	}
	req := avatarUploadRequest(t, "http://127.0.0.1:8090/avatars/avatar.png", testAvatarPNGWithColor(t, color.RGBA{R: 255, A: 255}))
	rr := httptest.NewRecorder()
	a.handleAvatarUpload(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	firstURL, err := url.Parse(response["url"])
	if err != nil || firstURL.Path != avatarImagePath || len(firstURL.Query().Get("v")) != 64 {
		t.Fatalf("avatar URL=%q", response["url"])
	}
	assertAvatarUpdateQueued(t, db, response["url"])
	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), "# preserve this comment") {
		t.Fatalf("config comment not preserved:\n%s", configData)
	}
	var cfg config.Config
	if err := yaml.Unmarshal(configData, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Game.AvatarURL != response["url"] || a.cfg.Game.AvatarURL != response["url"] {
		t.Fatalf("persisted/in-memory avatar URL mismatch: config=%q app=%q", cfg.Game.AvatarURL, a.cfg.Game.AvatarURL)
	}

	rr = httptest.NewRecorder()
	a.handleAvatarImage(rr, httptest.NewRequest(http.MethodGet, response["url"], nil))
	served, err := png.Decode(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("image response status=%d content-type=%q", rr.Code, rr.Header().Get("Content-Type"))
	}
	if _, err := png.DecodeConfig(bytes.NewReader(rr.Body.Bytes())); err != nil {
		t.Fatalf("served avatar is not a PNG: %v", err)
	}
	r, g, b, _ := served.At(0, 0).RGBA()
	if r != 0xffff || g != 0 || b != 0 {
		t.Fatalf("served first avatar pixel=(%x,%x,%x), want red", r, g, b)
	}

	req = avatarUploadRequest(t, response["url"], testAvatarPNGWithColor(t, color.RGBA{B: 255, A: 255}))
	rr = httptest.NewRecorder()
	a.handleAvatarUpload(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("second upload status=%d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["url"] == firstURL.String() {
		t.Fatalf("new avatar kept cached URL %q", response["url"])
	}
	assertAvatarUpdateQueued(t, db, response["url"])

	rr = httptest.NewRecorder()
	a.handleAvatarImage(rr, httptest.NewRequest(http.MethodGet, response["url"], nil))
	served, err = png.Decode(bytes.NewReader(rr.Body.Bytes()))
	if rr.Code != http.StatusOK || err != nil {
		t.Fatalf("second served avatar status=%d decode err=%v", rr.Code, err)
	}
	r, g, b, _ = served.At(0, 0).RGBA()
	if b != 0xffff || r != 0 || g != 0 {
		t.Fatalf("served second avatar pixel=(%x,%x,%x), want blue", r, g, b)
	}
}
func assertAvatarUpdateQueued(t *testing.T, db *store.DB, wantURL string) {
	t.Helper()
	raw, ok, err := db.QueuePop(context.Background())
	if err != nil || !ok {
		t.Fatalf("avatar update command missing: ok=%v err=%v", ok, err)
	}
	var cmd struct {
		Op   string `json:"op"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Op != "update_avatar" || cmd.Text != wantURL {
		t.Fatalf("avatar update command=%+v, want URL %q", cmd, wantURL)
	}
}
func TestHandleAvatarUploadRejectsInvalidImageAndCrossOrigin(t *testing.T) {
	a := &app{
		cfg:       &config.Config{Gateway: config.Gateway{AdvertiseHost: "127.0.0.1"}},
		cfgPath:   filepath.Join(t.TempDir(), "config.yaml"),
		avatarDir: filepath.Join(t.TempDir(), "avatars"),
	}
	if err := os.WriteFile(a.cfgPath, []byte("game: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := avatarUploadRequest(t, "", []byte("not an image"))
	rr := httptest.NewRecorder()
	a.handleAvatarUpload(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid image status=%d body=%s", rr.Code, rr.Body.String())
	}

	req = avatarUploadRequest(t, "", testAvatarPNG(t))
	req.Header.Set("Origin", "http://evil.example")
	rr = httptest.NewRecorder()
	a.handleAvatarUpload(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(a.avatarDir, "avatar.png")); !os.IsNotExist(err) {
		t.Fatalf("rejected upload created an avatar: err=%v", err)
	}
}

func avatarUploadRequest(t *testing.T, avatarURL string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("image", "avatar.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if avatarURL != "" {
		if err := writer.WriteField("url", avatarURL); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := panelPOSTRequest("/api/avatar/upload", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func testAvatarPNG(t *testing.T) []byte {
	return testAvatarPNGWithColor(t, color.RGBA{R: 255, A: 255})
}
func testAvatarPNGWithColor(t *testing.T, pixel color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, pixel)
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
