package gallery

import (
	"math"
	"sort"
	"strconv"
)

const noneTag = "__no_tags__"

type Cluster struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Tags  []string `json:"tags"`
	Count int      `json:"count"`
	Items []Media  `json:"items"`
}

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
		normalizeVec(v)
		vectors[i] = v
	}

	assignments := make([]int, n)
	centers := kmeansInit(vectors, k)

	for iter := 0; iter < 50; iter++ {
		changed := 0
		for i, v := range vectors {
			c := nearestCenter(v, centers)
			if assignments[i] != c {
				assignments[i] = c
				changed++
			}
		}
		if changed == 0 {
			break
		}
		for c := 0; c < k; c++ {
			count := 0
			for j := range centers {
				centers[j] = make([]float64, dim)
			}
			for i, v := range vectors {
				if assignments[i] == c {
					count++
					for j, x := range v {
						centers[c][j] += x
					}
				}
			}
			if count > 0 {
				for j := range centers[c] {
					centers[c][j] /= float64(count)
				}
			}
		}
	}

	clusters := make([]Cluster, k)
	for c := 0; c < k; c++ {
		clusters[c].ID = strconv.Itoa(c)
	}
	for i, m := range items {
		c := assignments[i]
		if len(clusters[c].Items) < itemLimit {
			clusters[c].Items = append(clusters[c].Items, *m)
		}
		clusters[c].Count++
	}

	for c := range clusters {
		tagCount := map[string]int{}
		for i, m := range items {
			if assignments[i] != c {
				continue
			}
			for _, t := range m.AllTags() {
				tagCount[t]++
			}
		}
		type tc struct {
			tag string
			n   int
		}
		top := make([]tc, 0, len(tagCount))
		for t, n := range tagCount {
			top = append(top, tc{t, n})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
		if len(top) > 0 {
			clusters[c].Label = top[0].tag
			for _, t := range top {
				clusters[c].Tags = append(clusters[c].Tags, t.tag)
				if len(clusters[c].Tags) >= 5 {
					break
				}
			}
		} else {
			clusters[c].Label = "未分类"
		}
	}

	out := make([]Cluster, 0, k)
	for _, cl := range clusters {
		if cl.Count > 0 {
			out = append(out, cl)
		}
	}
	return out
}

func normalizeVec(v []float64) {
	norm := 0.0
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		return
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] /= norm
	}
}

func kmeansInit(vectors [][]float64, k int) [][]float64 {
	n := len(vectors)
	centers := make([][]float64, k)
	for i := 0; i < k; i++ {
		centers[i] = make([]float64, len(vectors[i%n]))
		copy(centers[i], vectors[i%n])
	}
	return centers
}

func nearestCenter(v []float64, centers [][]float64) int {
	best := 0
	bestDist := -1.0
	for c, center := range centers {
		dist := 0.0
		for i, x := range v {
			d := x - center[i]
			dist += d * d
		}
		if bestDist < 0 || dist < bestDist {
			bestDist = dist
			best = c
		}
	}
	return best
}

func (g *Gallery) SortedMedia(sortKey string) []*Media {
	out := make([]*Media, len(g.media))
	copy(out, g.media)
	sortMedia(out, sortKey)
	return out
}
