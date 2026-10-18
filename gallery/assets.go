// Package gallery 提供 Twitter Pic 的 SSR 图库前端与 Web 交互路由。
//
// 本文件（assets.go）实现前端模板与静态资源的「双模加载器」：
//   1. 内嵌兜底（//go:embed）：编译进单一二进制，任何工作目录下（含单二进制部署、单元测试）开箱即用；
//   2. 磁盘实时热更新（Live Disk Reload）：面向频繁更新前端的开发与线上热更场景。
//      当检测到磁盘前端目录（GALLERY_FRONTEND_DIR，或默认 ./gallery / .）下的
//      templates/*.html、static/css/*.css、static/*.js 发生修改（mtime 变化）时，
//      无需重新编译 Go 二进制或重启进程，自动重新加载模板与 CSS/JS 并使首页 SSR 缓存失效，
//      浏览器刷新即刻生效。
package gallery

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

// cssModuleOrder 定义模块化 CSS 的合并注入顺序：
// base.css（变量/重置/顶栏/弹窗）→ home.css（首页）→ account.css（账号页）→ lightbox.css（全屏查看器/响应式）
var cssModuleOrder = []string{
	"css/base.css",
	"css/home.css",
	"css/account.css",
	"css/lightbox.css",
}

// frontendBundle 持有一份已编译的 HTML 模板与内联静态资源快照。
type frontendBundle struct {
	templates *template.Template
	appCSS    template.CSS
	homeJS    template.JS
	appJS     template.JS
	mtime     time.Time
}

// assetManager 管理内嵌资源与可选的磁盘热更新。
type assetManager struct {
	mu       sync.RWMutex
	diskDir  string
	embedded *frontendBundle
	current  *frontendBundle
}

var defaultAssets = newAssetManager()

func newAssetManager() *assetManager {
	emb := loadEmbeddedBundle()
	dir := resolveFrontendDiskDir()
	m := &assetManager{
		diskDir:  dir,
		embedded: emb,
		current:  emb,
	}
	if dir != "" {
		if b, mtime, err := loadDiskBundle(dir); err == nil {
			b.mtime = mtime
			m.current = b
		}
	}
	return m
}

func resolveFrontendDiskDir() string {
	if explicit := strings.TrimSpace(os.Getenv("GALLERY_FRONTEND_DIR")); explicit != "" {
		if hasFrontendFiles(explicit) {
			return explicit
		}
	}
	if strings.TrimSpace(os.Getenv("GALLERY_DEV")) == "0" {
		return ""
	}
	for _, cand := range []string{"./gallery", "."} {
		if hasFrontendFiles(cand) {
			return cand
		}
	}
	return ""
}

func hasFrontendFiles(dir string) bool {
	if dir == "" {
		return false
	}
	st1, err1 := os.Stat(filepath.Join(dir, "templates", "home.html"))
	st2, err2 := os.Stat(filepath.Join(dir, "static", "home.js"))
	return err1 == nil && !st1.IsDir() && err2 == nil && !st2.IsDir()
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"sub": func(a, b int) int { return a - b },
	}
}

func loadEmbeddedBundle() *frontendBundle {
	tmpl := template.Must(
		template.New("").Funcs(templateFuncs()).ParseFS(tmplFS, "templates/*.html"),
	)
	var cssBuilder strings.Builder
	for _, rel := range cssModuleOrder {
		if b, err := staticFS.ReadFile("static/" + rel); err == nil {
			cssBuilder.Write(b)
			cssBuilder.WriteByte('\n')
		}
	}
	return &frontendBundle{
		templates: tmpl,
		appCSS:    template.CSS(cssBuilder.String()),
		homeJS:    template.JS(mustReadEmbeddedStatic("home.js")),
		appJS:     template.JS(mustReadEmbeddedStatic("app.js")),
	}
}

func mustReadEmbeddedStatic(name string) string {
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		log.Printf("gallery: read embedded static %s: %v", name, err)
		return ""
	}
	return string(b)
}

func scanDiskMaxMtime(dir string) (time.Time, error) {
	var latest time.Time
	patterns := []string{
		filepath.Join(dir, "templates", "*.html"),
		filepath.Join(dir, "static", "*.js"),
		filepath.Join(dir, "static", "css", "*.css"),
	}
	for _, pat := range patterns {
		matches, err := filepath.Glob(pat)
		if err != nil {
			return latest, err
		}
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				return latest, err
			}
			if info.ModTime().After(latest) {
				latest = info.ModTime()
			}
		}
	}
	return latest, nil
}

func loadDiskBundle(dir string) (*frontendBundle, time.Time, error) {
	mtime, err := scanDiskMaxMtime(dir)
	if err != nil {
		return nil, time.Time{}, err
	}
	tmpl, err := template.New("").Funcs(templateFuncs()).ParseGlob(filepath.Join(dir, "templates", "*.html"))
	if err != nil {
		return nil, time.Time{}, err
	}
	var cssBuilder strings.Builder
	for _, rel := range cssModuleOrder {
		p := filepath.Join(dir, "static", filepath.FromSlash(rel))
		if b, err := os.ReadFile(p); err == nil {
			cssBuilder.Write(b)
			cssBuilder.WriteByte('\n')
		}
	}
	homeBytes, err := os.ReadFile(filepath.Join(dir, "static", "home.js"))
	if err != nil {
		return nil, time.Time{}, err
	}
	appBytes, err := os.ReadFile(filepath.Join(dir, "static", "app.js"))
	if err != nil {
		return nil, time.Time{}, err
	}
	return &frontendBundle{
		templates: tmpl,
		appCSS:    template.CSS(cssBuilder.String()),
		homeJS:    template.JS(string(homeBytes)),
		appJS:     template.JS(string(appBytes)),
		mtime:     mtime,
	}, mtime, nil
}

// bundle 获取当前最新的前端资源快照；若磁盘文件有更新则自动热重载并返回 changed=true。
func (m *assetManager) bundle() (b *frontendBundle, changed bool) {
	if m == nil {
		return loadEmbeddedBundle(), false
	}
	m.mu.RLock()
	dir := m.diskDir
	cur := m.current
	m.mu.RUnlock()

	if dir == "" {
		return cur, false
	}
	mtime, err := scanDiskMaxMtime(dir)
	if err != nil || !mtime.After(cur.mtime) {
		return cur, false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !mtime.After(m.current.mtime) {
		return m.current, false
	}
	fresh, newMtime, err := loadDiskBundle(dir)
	if err != nil {
		log.Printf("gallery: 前端磁盘热重载失败（保留上一版本）: %v", err)
		return m.current, false
	}
	fresh.mtime = newMtime
	m.current = fresh
	log.Printf("gallery: 检测到前端文件变更，已从 %s 热重载模板与静态资源", dir)
	return fresh, true
}

// staticHandler 返回 /static/* 的文件处理器：优先服务磁盘目录（实时反映修改），回退至 embed.FS。
func (m *assetManager) staticHandler() http.Handler {
	var embeddedHandler http.Handler
	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		embeddedHandler = http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m != nil && m.diskDir != "" {
			diskStatic := filepath.Join(m.diskDir, "static")
			if st, err := os.Stat(diskStatic); err == nil && st.IsDir() {
				w.Header().Set("Cache-Control", "no-cache")
				http.StripPrefix("/static/", http.FileServer(http.Dir(diskStatic))).ServeHTTP(w, r)
				return
			}
		}
		if embeddedHandler != nil {
			embeddedHandler.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func render(w http.ResponseWriter, name string, data pageData) {
	b, _ := defaultAssets.bundle()
	if data.AppCSS == "" {
		data.AppCSS = b.appCSS
	}
	if data.AppJS == "" {
		data.AppJS = b.appJS
	}
	if data.HomeJS == "" {
		data.HomeJS = b.homeJS
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := b.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("gallery: render %s: %v", name, err)
	}
}

func writeHTML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(body); err != nil {
		log.Printf("gallery: write html: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
