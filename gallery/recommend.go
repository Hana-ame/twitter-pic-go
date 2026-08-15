package main

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
	"time"
)

// tagProfile builds a weighted tag profile from recent history.
// Weights decay with a 24 hour half-life so recent views dominate.
func tagProfile(entries []HistoryEntry, now time.Time) map[string]float64 {
	weights := map[string]float64{}
	for _, e := range entries {
		ageHours := now.Sub(e.ViewedAt).Hours()
		if ageHours < 0 {
			ageHours = 0
		}
		w := math.Exp(-math.Ln2 * ageHours / 24.0)
		for _, t := range e.Tags {
			weights[t] += w
		}
	}
	return weights
}

func dot(a map[string]float64, media *Media) float64 {
	s := 0.0
	for _, t := range media.AllTags() {
		s += a[t]
	}
	return s
}

// Recommend returns media items scored by tag affinity with recent history.
func (g *Gallery) Recommend(h *HistoryStore, limit int) []Media {
	items := g.SortedMedia("name")
	if limit <= 0 || limit > len(items) {
		limit = len(items)
	}
	if len(items) == 0 {
		return nil
	}

	entries := h.Recent(100)
	if len(entries) == 0 {
		// No history yet: return newest media first.
		items = g.SortedMedia("time")
		if limit > len(items) {
			limit = len(items)
		}
		return copyMedia(items[:limit])
	}

	profile := tagProfile(entries, time.Now())

	// Exclude media that were just viewed so recommendations are not stale.
	exclude := map[string]bool{}
	recentViews := h.Recent(20)
	for _, e := range recentViews {
		exclude[e.MediaID] = true
	}

	type scored struct {
		m     *Media
		score float64
	}
	list := make([]scored, 0, len(items))
	for _, m := range items {
		if exclude[m.ID] {
			continue
		}
		list = append(list, scored{m: m, score: dot(profile, m)})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].m.Name < list[j].m.Name
	})

	if len(list) == 0 {
		list = make([]scored, 0, len(items))
		for _, m := range items {
			list = append(list, scored{m: m, score: dot(profile, m)})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].score != list[j].score {
				return list[i].score > list[j].score
			}
			return list[i].m.Name < list[j].m.Name
		})
	}

	if limit > len(list) {
		limit = len(list)
	}
	out := make([]Media, 0, limit)
	for _, s := range list[:limit] {
		out = append(out, *s.m)
	}
	return out
}

// Related returns media most similar to the given media by tag Jaccard index.
func (g *Gallery) Related(m *Media, limit int) []Media {
	if limit <= 0 {
		limit = 10
	}
	tags := map[string]bool{}
	for _, t := range m.AllTags() {
		tags[t] = true
	}

	type scored struct {
		m     *Media
		score float64
	}
	var list []scored

	for _, other := range g.SortedMedia("name") {
		if other.ID == m.ID {
			continue
		}
		inter := 0
		union := len(tags)
		seen := map[string]bool{}
		for t := range tags {
			seen[t] = true
		}
		for _, t := range other.AllTags() {
			if seen[t] {
				inter++
			} else {
				union++
			}
		}
		if union == 0 {
			continue
		}
		score := float64(inter) / float64(union)
		if score > 0 {
			list = append(list, scored{m: other, score: score})
		}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].score != list[j].score {
			return list[i].score > list[j].score
		}
		return list[i].m.Name < list[j].m.Name
	})

	// Fallback: same directory items if there is no tag overlap.
	if len(list) == 0 {
		for _, other := range g.SortedMedia("name") {
			if other.ID != m.ID && other.Dir == m.Dir {
				list = append(list, scored{m: other, score: 0.01})
			}
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].score != list[j].score {
				return list[i].score > list[j].score
			}
			return list[i].m.Name < list[j].m.Name
		})
	}

	if limit > len(list) {
		limit = len(list)
	}
	out := make([]Media, 0, limit)
	for _, s := range list[:limit] {
		out = append(out, *s.m)
	}
	return out
}

const noneTag = "__no_tags__"

// Cluster represents a group of similar media produced by k-means.
type Cluster struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Tags  []string `json:"tags"`
	Count int      `json:"count"`
	Items []Media  `json:"items"`
}

// Clusters runs a small k-means over tag vectors and returns up to k clusters.
func (g *Gallery) Clusters(k, itemLimit int) []Cluster {
	items := g.SortedMedia("name")
	n := len(items)
	if n == 0 {
		return []Cluster{}
	}
	if k <= 0 {
		k = 6
	}
	if k > n {
		k = n
	}
	if itemLimit <= 0 {
		itemLimit = 12
	}

	// Build a tag vocabulary. A special tag keeps zero-tag items usable.
	vocab := map[string]int{}
	for _, m := range items {
		tags := m.AllTags()
		if len(tags) == 0 {
			vocab[noneTag] = 0
			continue
		}
		for _, t := range tags {
			vocab[t] = 0
		}
	}
	tagNames := make([]string, 0, len(vocab))
	for t := range vocab {
		vocab[t] = len(tagNames)
		tagNames = append(tagNames, t)
	}
	dim := len(tagNames)

	vectors := make([][]float64, n)
	for i, m := range items {
		v := make([]float64, dim)
		tags := m.AllTags()
		if len(tags) == 0 {
			v[vocab[noneTag]] = 1
		} else {
			for _, t := range tags {
				if idx, ok := vocab[t]; ok {
					v[idx] = 1
				}
			}
		}
		normalize(v)
		vectors[i] = v
	}

	assignments := make([]int, n)
	centers := kmeansInit(vectors, k)

	for iter := 0; iter < 50; iter++ {
		changed := 0
		for i, v := range vectors {
			c := nearestCenter(v, centers)
			if assignments[i] != c {
				changed++
				assignments[i] = c
			}
		}
		if changed == 0 {
			break
		}
		centers = recomputeCenters(vectors, assignments, k)
	}

	// Build clusters.
	clusters := make([]Cluster, 0, k)
	for c := 0; c < k; c++ {
		var members []*Media
		for i, a := range assignments {
			if a == c {
				members = append(members, items[i])
			}
		}
		if len(members) == 0 {
			continue
		}
		topTags := topClusterTags(members, 10)
		label := ""
		if len(topTags) > 0 {
			label = topTags[0]
		}
		if len(topTags) > 1 {
			label += " / " + topTags[1]
		}
		if label == "" {
			label = "cluster"
		}
		cluster := Cluster{
			ID:    string(rune('A' + c)),
			Label: label,
			Tags:  topTags,
			Count: len(members),
		}
		sort.Slice(members, func(i, j int) bool {
			return members[i].Name < members[j].Name
		})
		for i, m := range members {
			if i >= itemLimit {
				break
			}
			cluster.Items = append(cluster.Items, *m)
		}
		clusters = append(clusters, cluster)
	}

	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].Count > clusters[j].Count
	})
	for i := range clusters {
		if i < 26 {
			clusters[i].ID = string(rune('A' + i))
		} else {
			clusters[i].ID = "C" + strconv.Itoa(i+1)
		}
	}
	return clusters
}

func normalize(v []float64) {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return
	}
	norm := math.Sqrt(sum)
	for i := range v {
		v[i] /= norm
	}
}

func cosine(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func nearestCenter(v []float64, centers [][]float64) int {
	best := 0
	bestScore := math.Inf(-1)
	for i, c := range centers {
		s := cosine(v, c)
		if s > bestScore {
			bestScore = s
			best = i
		}
	}
	return best
}

func kmeansInit(vectors [][]float64, k int) [][]float64 {
	rng := rand.New(rand.NewSource(42))
	centers := make([][]float64, k)
	for i := range centers {
		centers[i] = make([]float64, len(vectors[0]))
	}
	// First center: copy the first vector.
	copy(centers[0], vectors[0])
	distSq := make([]float64, len(vectors))
	for c := 1; c < k; c++ {
		var sum float64
		for i, v := range vectors {
			d := 1 - cosine(v, centers[nearestCenter(v, centers[:c])])
			distSq[i] = d * d
			sum += distSq[i]
		}
		if sum <= 0 {
			// Deterministic fallback.
			copy(centers[c], vectors[c%len(vectors)])
			continue
		}
		r := rng.Float64() * sum
		chosen := 0
		for i, d := range distSq {
			r -= d
			if r <= 0 {
				chosen = i
				break
			}
		}
		copy(centers[c], vectors[chosen])
	}
	return centers
}

func recomputeCenters(vectors [][]float64, assignments []int, k int) [][]float64 {
	dim := len(vectors[0])
	centers := make([][]float64, k)
	counts := make([]int, k)
	for i := range centers {
		centers[i] = make([]float64, dim)
	}
	for i, v := range vectors {
		c := assignments[i]
		counts[c]++
		for j := range v {
			centers[c][j] += v[j]
		}
	}
	for c := 0; c < k; c++ {
		if counts[c] == 0 {
			// Re-seed empty cluster with the point farthest from its own center.
			best := 0
			bestDist := math.Inf(1)
			for i, v := range vectors {
				own := centers[assignments[i]]
				d := 1 - cosine(v, own)
				if d < bestDist {
					bestDist = d
					best = i
				}
			}
			copy(centers[c], vectors[best])
			continue
		}
		for j := range centers[c] {
			centers[c][j] /= float64(counts[c])
		}
		normalize(centers[c])
	}
	return centers
}

func topClusterTags(members []*Media, limit int) []string {
	counts := map[string]int{}
	for _, m := range members {
		seen := map[string]bool{}
		for _, t := range m.AllTags() {
			if t == noneTag {
				continue
			}
			if !seen[t] {
				seen[t] = true
				counts[t]++
			}
		}
	}
	type tc struct {
		tag   string
		count int
	}
	list := make([]tc, 0, len(counts))
	for t, c := range counts {
		list = append(list, tc{t, c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		return list[i].tag < list[j].tag
	})
	if limit > len(list) {
		limit = len(list)
	}
	out := make([]string, 0, limit)
	for _, x := range list[:limit] {
		out = append(out, x.tag)
	}
	return out
}

// SortedMedia returns a copy of the media slice sorted by the requested key.
func (g *Gallery) SortedMedia(sortKey string) []*Media {
	out := make([]*Media, len(g.media))
	copy(out, g.media)
	switch sortKey {
	case "time":
		sort.Slice(out, func(i, j int) bool {
			if !out[i].ModTime.Equal(out[j].ModTime) {
				return out[i].ModTime.After(out[j].ModTime)
			}
			return out[i].Name < out[j].Name
		})
	case "size":
		sort.Slice(out, func(i, j int) bool {
			if out[i].Size != out[j].Size {
				return out[i].Size > out[j].Size
			}
			return out[i].Name < out[j].Name
		})
	default:
		sort.Slice(out, func(i, j int) bool {
			return out[i].Name < out[j].Name
		})
	}
	return out
}

func copyMedia(items []*Media) []Media {
	out := make([]Media, 0, len(items))
	for _, m := range items {
		out = append(out, *m)
	}
	return out
}
