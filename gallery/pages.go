package gallery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Hana-ame/twitter-pic-go/tags"
)

type mediaItem struct {
	URL     string
	OuoURL  string
	IsVideo bool
	TweetID int64
	Cover   string
}

type accountPage struct {
	Slug      string
	Name      string
	Nick      string
	Avatar    string
	Banner    string
	Media     []mediaItem
	Total     int
	Filter    string
	Cursor    int64
	PageSize  int
	HasPrev   bool
	HasNext   bool
	HrefAll   string
	HrefPhoto string
	HrefVideo string
	PrevHref  string
	NextHref  string
	RawJSON   template.JS
	ATagsJSON template.JS
	MediaBase string
	VideoBase string
	OuoPrefix string
	LegacyURL string
}

type acctItem struct {
	Name    string
	Initial string
	Hue     int
	Tags    []string
}

type acctEntry struct {
	Name string   `json:"n"`
	Tags []string `json:"t,omitempty"`
	U    string   `json:"u,omitempty"`
}

type homeData struct {
	Total     int
	Preview   []acctItem
	Cloud     []tags.Count
	Untagged  int
	NamesJSON template.JS
	MediaBase string
}

type pageData struct {
	Title      string
	Home       *homeData
	Account    *accountPage
	AppCSS     template.CSS
	AppJS      template.JS
	HomeJS     template.JS
	LegacyBase string
}

func hueOf(s string) int {
	h := 0
	for i := 0; i < len(s); i++ {
		h = (h*31 + int(s[i])) % 360
	}
	return h
}

func handleHome(w http.ResponseWriter, r *http.Request, cfg config) {
	body, err := homeHTML(cfg)
	if err != nil {
		log.Printf("gallery: 首页构造失败: %v", err)
		http.Error(w, "home: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeHTML(w, body)
}

func homeHTML(cfg config) ([]byte, error) {
	if _, changed := defaultAssets.bundle(); changed {
		if cfg.home != nil {
			cfg.home.invalidate()
			return cfg.home.buildOnce()
		}
	}
	if cfg.home == nil {
		return buildHomeHTML(cfg)
	}
	return cfg.home.html()
}

func buildHomeHTML(cfg config) ([]byte, error) {
	names, err := listAccounts(cfg.jsonDir)
	if err != nil {
		return nil, fmt.Errorf("read json dir: %w", err)
	}
	names = cfg.vis.filter(names)
	const previewN = 120
	tagged := cfg.tags.ForUsers(names)
	lm := cfg.tags.LastModifyFor(names)
	entries := make([]acctEntry, 0, len(names))
	untagged := 0
	for _, n := range names {
		t := tagged[n]
		entries = append(entries, acctEntry{Name: n, Tags: t, U: lm[n]})
		if len(t) == 0 {
			untagged++
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].U != entries[j].U {
			return entries[i].U > entries[j].U
		}
		return entries[i].Name < entries[j].Name
	})
	top := cfg.vis.cloud()
	if top == nil && cfg.tags != nil {
		top = cfg.tags.Cloud(cloudTopN, true)
	}
	preview := make([]acctItem, 0, previewN)
	for _, e := range entries[:min(len(entries), previewN)] {
		initial := "?"
		if e.Name != "" {
			initial = strings.ToUpper(e.Name[:1])
		}
		preview = append(preview, acctItem{Name: e.Name, Initial: initial, Hue: hueOf(e.Name), Tags: e.Tags})
	}
	dataJSON, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("marshal entries: %w", err)
	}
	b, _ := defaultAssets.bundle()
	var buf bytes.Buffer
	if err := b.templates.ExecuteTemplate(&buf, "home.html", pageData{
		Title: "首页",
		Home: &homeData{
			Total: len(names), Preview: preview, Cloud: top, Untagged: untagged,
			NamesJSON: template.JS(dataJSON), MediaBase: cfg.mediaBase,
		},
		AppCSS:     b.appCSS,
		HomeJS:     b.homeJS,
		LegacyBase: cfg.legacyBase,
	}); err != nil {
		return nil, fmt.Errorf("render home.html: %w", err)
	}
	return buf.Bytes(), nil
}

func handleAccount(w http.ResponseWriter, r *http.Request, cfg config) {
	slug := r.PathValue("account")
	if !safeName(slug) || cfg.vis.hidden(slug) {
		http.NotFound(w, r)
		return
	}
	doc, err := loadDocument(filepath.Join(cfg.jsonDir, slug+".json.gz"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	typeParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	var filter string
	switch typeParam {
	case "all", "everything":
		filter = "all"
	case "video", "videos", "movie", "animated_gif", "gif":
		filter = "video"
	default:
		filter = "photo"
	}

	ouoPrefix := cfg.ouoPrefix
	if ouoPrefix == "" {
		ouoPrefix = resolveOuoPrefix()
	}

	list := filterMedia(buildAll(doc, cfg.mediaBase, cfg.videoBase, ouoPrefix), filter)

	start := clampStart(list, int(parseCursorID(r.URL.Query().Get("cursor"))), cfg.pageSize)
	end := start + cfg.pageSize
	if end > len(list) {
		end = len(list)
	}

	var prevCursor, nextCursor int64
	if start > 0 {
		prevCursor = int64(clampStart(list, start-cfg.pageSize, cfg.pageSize))
	}
	if end < len(list) {
		nextCursor = int64(end)
	}

	for i := range doc.Timeline {
		if doc.Timeline[i].Type == "video" || doc.Timeline[i].Type == "animated_gif" {
			doc.Timeline[i].URL = overrideVideoURL(cfg.videoBase, doc.Timeline[i].URL)
			doc.Timeline[i].Cover = mediaURL(cfg.mediaBase, strings.TrimSpace(doc.Timeline[i].Cover))
		}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		raw = []byte("{}")
	}

	var atagJSON []byte
	if b, err := json.Marshal(cfg.tags.Weights(slug)); err == nil {
		atagJSON = b
	}
	if atagJSON == nil {
		atagJSON = []byte("{}")
	}

	acc := &accountPage{
		Slug:      slug,
		Name:      firstNonEmpty(strings.TrimSpace(doc.AccountInfo.Name), slug),
		Nick:      strings.TrimSpace(doc.AccountInfo.Nick),
		Avatar:    mediaURL(cfg.mediaBase, doc.AccountInfo.ProfileImage),
		Banner:    mediaURL(cfg.mediaBase, strings.TrimSpace(doc.AccountInfo.ProfileBanner)),
		Media:     list[start:end],
		Total:     len(list),
		Filter:    filter,
		Cursor:    int64(start),
		PageSize:  cfg.pageSize,
		HasPrev:   start > 0,
		HasNext:   end < len(list),
		HrefAll:   buildHref(slug, "all", 0),
		HrefPhoto: buildHref(slug, "photo", 0),
		HrefVideo: buildHref(slug, "video", 0),
		PrevHref:  buildHref(slug, filter, prevCursor),
		NextHref:  buildHref(slug, filter, nextCursor),
		RawJSON:   template.JS(raw),
		ATagsJSON: template.JS(atagJSON),
		MediaBase: cfg.mediaBase,
		VideoBase: cfg.videoBase,
		OuoPrefix: ouoPrefix,
		LegacyURL: legacyURL(cfg.legacyBase, slug),
	}
	render(w, "account.html", pageData{Title: acc.Name, Account: acc, LegacyBase: cfg.legacyBase})
}
