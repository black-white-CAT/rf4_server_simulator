package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxAvatarUploadBytes = 5 << 20
	maxAvatarPixels      = 16_000_000
	avatarImagePath      = "/avatars/avatar.png"
)

func (a *app) handleAvatarUpload(w http.ResponseWriter, r *http.Request) {
	if !requireLocalPost(w, r, "multipart/form-data") {
		return
	}
	// multipart 分隔符和表单字段也占用请求体，因此请求上限需略高于图片上限。
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarUploadBytes+(64<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "上传数据无效或超过 5 MiB"})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}

	file, _, err := r.FormFile("image")
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "请选择要上传的 PNG、JPEG 或 GIF 图片"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxAvatarUploadBytes+1))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "读取上传图片失败"})
		return
	}
	if len(data) == 0 || len(data) > maxAvatarUploadBytes {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "图片为空或超过 5 MiB"})
		return
	}
	pngData, err := normalizeAvatarPNG(data)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	avatarURL := strings.TrimSpace(r.FormValue("url"))
	if avatarURL == "" {
		avatarURL = a.suggestedAvatarURL()
	}
	if err := validateAvatarURL(avatarURL); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 将图片摘要写入 URL，避免客户端继续使用旧头像缓存。
	avatarURL, err = versionAvatarURL(avatarURL, pngData)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if a.cfgPath == "" {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "未指定配置文件路径"})
		return
	}

	a.avatarMu.Lock()
	defer a.avatarMu.Unlock()
	// 串行化文件与配置更新，避免并发上传留下不一致状态。
	dir := a.avatarDir
	if dir == "" {
		dir = "avatars"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "创建头像目录失败: " + err.Error()})
		return
	}
	if err := replaceAvatarFile(filepath.Join(dir, "avatar.png"), pngData); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "保存头像失败: " + err.Error()})
		return
	}
	if err := persistAvatarURL(a.cfgPath, avatarURL); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "头像已上传，但保存配置失败: " + err.Error()})
		return
	}

	if a.cfg != nil {
		a.cfg.Game.AvatarURL = avatarURL
	}
	if err := a.enqueueAvatarUpdate(avatarURL); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]string{
			"error": "头像已保存，但在线刷新指令投递失败，请重启 gameserver 后生效: " + err.Error(),
		})
		return
	}
	writeJSON(w, map[string]string{"url": avatarURL})
}
func (a *app) enqueueAvatarUpdate(avatarURL string) error {
	a.adminMu.RLock()
	defer a.adminMu.RUnlock()
	if a.adminDB == nil {
		return fmt.Errorf("管理数据库尚未就绪")
	}
	payload, err := json.Marshal(struct {
		Op   string `json:"op"`
		Text string `json:"text"`
	}{Op: "update_avatar", Text: avatarURL})
	if err != nil {
		return fmt.Errorf("编码头像刷新指令: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.adminDB.QueuePush(ctx, string(payload)); err != nil {
		return fmt.Errorf("写入管理指令队列: %w", err)
	}
	return nil
}

func (a *app) handleAvatarImage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != avatarImagePath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "仅支持 GET 和 HEAD", http.StatusMethodNotAllowed)
		return
	}

	dir := a.avatarDir
	if dir == "" {
		dir = "avatars"
	}
	file, err := os.Open(filepath.Join(dir, "avatar.png"))
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "读取头像失败", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "读取头像信息失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "avatar.png", info.ModTime(), file)
}

func (a *app) suggestedAvatarURL() string {
	host := ""
	if a.cfg != nil {
		host = strings.Trim(strings.TrimSpace(a.cfg.Gateway.AdvertiseHost), "[]")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	port := a.avatarPort
	if port <= 0 {
		port = 8090
	}
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   avatarImagePath,
	}).String()
}

func validateAvatarURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.User != nil ||
		u.Path != avatarImagePath || u.Fragment != "" {
		return fmt.Errorf("头像地址必须是完整的 HTTP 地址，并以 %s 结尾", avatarImagePath)
	}
	query := u.Query()
	if len(query) > 0 {
		version := query.Get("v")
		if len(query) != 1 || len(version) != sha256.Size*2 {
			return fmt.Errorf("头像地址中的版本参数无效")
		}
		if _, err := hex.DecodeString(version); err != nil {
			return fmt.Errorf("头像地址中的版本参数无效")
		}
	}
	return nil
}
func versionAvatarURL(raw string, image []byte) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析头像地址失败: %w", err)
	}
	sum := sha256.Sum256(image)
	query := u.Query()
	query.Set("v", hex.EncodeToString(sum[:]))
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func normalizeAvatarPNG(data []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg" && format != "gif") {
		return nil, fmt.Errorf("图片格式无效，仅支持 PNG、JPEG 或 GIF")
	}
	// 完整解码前先检查像素总量，避免为超大图片分配内存。
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxAvatarPixels/cfg.Height {
		return nil, fmt.Errorf("图片尺寸无效或超过 1600 万像素")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("图片内容无法解码")
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, fmt.Errorf("转换 PNG 失败: %w", err)
	}
	if out.Len() > maxAvatarUploadBytes {
		return nil, fmt.Errorf("转换后的 PNG 超过 5 MiB")
	}
	return out.Bytes(), nil
}

func replaceAvatarFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".avatar-*.png")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	// 临时文件与目标文件放在同一目录，确保重命名发生在同一文件系统内。
	if err := os.Rename(tmpName, path); err == nil {
		return nil
	} else if _, statErr := os.Lstat(path); statErr != nil {
		return err
	}
	// 某些平台不能直接覆盖已有文件，失败时显式删除后再重命名。
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func persistAvatarURL(configPath, avatarURL string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(configPath)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if strings.TrimSpace(string(data)) != "" {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return err
		}
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = append(doc.Content, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("配置根节点不是 YAML 映射")
	}
	// 直接修改 YAML 节点树，避免解码到结构体时丢失无关字段和注释。
	game := mappingValue(root, "game")
	if game == nil {
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "game"},
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"},
		)
		game = root.Content[len(root.Content)-1]
	}
	if game.Kind != yaml.MappingNode {
		return fmt.Errorf("配置项 game 不是 YAML 映射")
	}
	setMappingValue(game, "avatar_url", avatarURL)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, out, info.Mode().Perm())
}

func requireLocalPost(w http.ResponseWriter, r *http.Request, expectedMediaType string) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != expectedMediaType {
		writeJSONStatus(w, http.StatusUnsupportedMediaType, map[string]string{"error": "请求 Content-Type 无效"})
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "仅允许本机控制中心调用"})
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			writeJSONStatus(w, http.StatusForbidden, map[string]string{"error": "拒绝跨站请求"})
			return false
		}
	}
	return true
}
