package gallery

// recommend.go 提供 /clusters 聚类页的后端逻辑。
//
// 图库没有可用的 embedding 服务，所以聚类走「标签向量的 k-means」：
// 每张媒体用它的标签集合编码成二值向量，归一化后跑 k-means。
// 簇标签取簇内出现次数最多的标签 —— 对「这类内容都长什么样」这个
// 浏览型问题足够用，而且完全离线、无外部依赖。
//
// 关键点：向量用的是 Media.ClusterTags()（账号 tag + per-image tag），
// 刻意排除 DirTags。DirTags 就是账号名，对每条媒体都相同，
// 算进去会让聚类退化成「按账号分组」，和账号列表页完全重复。
//
// 全部是纯函数式计算，不触碰 s.mu：handler 先用 s.current() 拿到
// 不可变快照，快照里的 g.media 切片在整个请求内是稳定的。

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
)

// noneTag 是没有任何标签的媒体的占位维度，保证它们能被聚到一个簇里，
// 而不是因为零向量全部落在某个任意中心上。
const noneTag = "__no_tags__"

// Cluster 是一组共享标签模式的媒体。
type Cluster struct {
	ID    string   `json:"id"`
	Label string   `json:"label"` // 簇内出现最多的标签；无标签簇为「未分类」
	Tags  []string `json:"tags"`  // 簇内出现最多的前几个标签
	Count int      `json:"count"` // 簇内媒体总数
	Items []Media  `json:"items"` // 预览用的前 itemLimit 个媒体
}

// SortedMedia 返回 g.media 的排序副本，供聚类稳定遍历。
func (g *Gallery) SortedMedia(sortKey string) []*Media {
	out := make([]*Media, len(g.media))
	copy(out, g.media)
	sortMedia(out, sortKey)
	return out
}

// Clusters 对全部媒体做 k-means 聚类，返回按簇内媒体数降序排列的簇列表。
//
//	k         簇数量，<=0 时用 6；超过媒体数时收敛到媒体数
//	itemLimit 每个簇预览的媒体数，<=0 时用 12
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

	// —— 词汇表：标签 -> 维度下标 ——
	vocab := map[string]int{}
	for _, m := range items {
		for _, t := range m.ClusterTags() {
			vocab[t] = 0
		}
	}
	vocab[noneTag] = 0
	tagNames := make([]string, 0, len(vocab))
	for t := range vocab {
		vocab[t] = len(tagNames)
		tagNames = append(tagNames, t)
	}
	dim := len(tagNames)

	// —— 标签向量（L2 归一化）——
	vectors := make([][]float64, n)
	for i, m := range items {
		v := make([]float64, dim)
		tags := m.ClusterTags()
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

	// —— k-means 迭代（确定性种子，保证同数据出同样结果）——
	// 所有随机性都走这一个 rng：k-means++ 初始化和空簇重挑都用它，
	// 所以整条链路可复现，便于结果缓存与回归对比。
	seed := clusterSeed(items)
	rng := rand.New(rand.NewSource(seed))

	assignments := make([]int, n)
	centers := kmeansPPInit(vectors, k, rng)
	for iter := 0; iter < 50; iter++ {
		changed := 0
		for i, v := range vectors {
			if c := nearestCenter(v, centers); assignments[i] != c {
				assignments[i] = c
				changed++
			}
		}
		if changed == 0 {
			break
		}
		for c := 0; c < k; c++ {
			centers[c] = make([]float64, dim)
			count := 0
			for i, v := range vectors {
				if assignments[i] != c {
					continue
				}
				count++
				for j, x := range v {
					centers[c][j] += x
				}
			}
			if count > 0 {
				for j := range centers[c] {
					centers[c][j] /= float64(count)
				}
			} else {
				// 空簇：把中心挪到一个还没被别的中心占用的样本上。
				// 随机挑一个样本可能又是已占用中心，导致空簇永远吸不到点。
				if src, ok := pickUnusedVector(vectors, centers[:c], rng); ok {
					copy(centers[c], src)
				} else if src := vectors[rng.Intn(n)]; len(src) == dim {
					copy(centers[c], src)
				}
			}
		}
	}

	// —— 组装簇 ——
	clusters := make([]Cluster, k)
	for c := range clusters {
		clusters[c].ID = strconv.Itoa(c)
		clusters[c].Items = []Media{}
	}
	for i, m := range items {
		c := assignments[i]
		if len(clusters[c].Items) < itemLimit {
			clusters[c].Items = append(clusters[c].Items, *m)
		}
		clusters[c].Count++
	}
	for c := range clusters {
		label, top := topTags(items, assignments, c, 5)
		clusters[c].Tags = top
		if label != "" {
			clusters[c].Label = label
		} else {
			clusters[c].Label = "未分类"
		}
	}

	// 非空簇按簇内媒体数降序，最「典型」的簇排前面。
	out := make([]Cluster, 0, k)
	for _, cl := range clusters {
		if cl.Count > 0 {
			out = append(out, cl)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// topTags 统计簇 c 内每个标签的出现次数，返回最多的标签及其前 maxTags 个标签名。
// 同频时按标签名字典序，保证输出稳定。
func topTags(items []*Media, assignments []int, c, maxTags int) (string, []string) {
	tagCount := map[string]int{}
	for i, m := range items {
		if assignments[i] != c {
			continue
		}
		for _, t := range m.ClusterTags() {
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
	sort.Slice(top, func(i, j int) bool {
		if top[i].n != top[j].n {
			return top[i].n > top[j].n
		}
		return top[i].tag < top[j].tag
	})
	if len(top) == 0 {
		return "", nil
	}
	if maxTags < 0 {
		maxTags = len(top)
	}
	if maxTags > len(top) {
		maxTags = len(top)
	}
	tags := make([]string, 0, maxTags)
	for _, t := range top[:maxTags] {
		tags = append(tags, t.tag)
	}
	return top[0].tag, tags
}

// normalizeVec 把向量归一化到单位长度；零向量原样返回。
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

// kmeansPPInit 用 k-means++ 抽样挑初始中心：第一个中心随机，
// 之后每个中心按「到已选中心的最小距离平方」加权随机抽取。
//
// 相比轮转抽样（vectors[i%n]），它有两个实际好处：
//  1. 轮转会反复选中同下标样本，同下标的向量往往一模一样，
//     产生重复中心；重复中心会让 tie 全落到同一个簇，另一个簇永远为空。
//  2. 距离加权让中心天然散布到不同标签模式上，
//     避免「所有点都被吸进一个簇、其余簇被丢弃」的塌缩。
//
// 退化成「全部向量相同」时（total==0），只能重复取，聚类结果退化为 1 个簇 ——
// 这是数据本身没有区分度，不是实现问题。
func kmeansPPInit(vectors [][]float64, k int, rng *rand.Rand) [][]float64 {
	n := len(vectors)
	k = minInt(k, n)
	centers := make([][]float64, 0, k)

	first := vectors[rng.Intn(n)]
	c0 := make([]float64, len(first))
	copy(c0, first)
	centers = append(centers, c0)

	for len(centers) < k {
		// 每个点到「已选中心里最近的那个」的距离平方。
		d := make([]float64, n)
		for i, v := range vectors {
			d[i] = minSqDistTo(v, centers)
		}
		total := 0.0
		for _, x := range d {
			total += x
		}

		var pick int
		if total == 0 {
			// 所有点都已精确落在某个中心上：无法再分散，随机兜底。
			pick = rng.Intn(n)
		} else {
			target := rng.Float64() * total
			acc := 0.0
			pick = n - 1
			for i, x := range d {
				acc += x
				if acc >= target {
					pick = i
					break
				}
			}
		}
		c := make([]float64, len(vectors[pick]))
		copy(c, vectors[pick])
		centers = append(centers, c)
	}
	return centers
}

// pickUnusedVector 从 vectors 里随机挑一个，要求它的向量与 used 中所有中心都不相同。
// 找不到时返回 ok=false，由调用方决定兜底策略。
func pickUnusedVector(vectors [][]float64, used [][]float64, rng *rand.Rand) ([]float64, bool) {
	n := len(vectors)
	if n == 0 {
		return nil, false
	}
	// 打乱顺序抽样，最坏 O(n²) 但簇数很小，实际很快。
	perm := rng.Perm(n)
	for _, i := range perm {
		v := vectors[i]
		dup := false
		for _, u := range used {
			if eqVec(v, u) {
				dup = true
				break
			}
		}
		if !dup {
			return v, true
		}
	}
	return nil, false
}

func eqVec(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// nearestCenter 返回距离 v 最近的中心下标（欧氏距离平方）。
func nearestCenter(v []float64, centers [][]float64) int {
	best := 0
	bestDist := math.Inf(1)
	for c, center := range centers {
		dist := 0.0
		for i, x := range v {
			d := x - center[i]
			dist += d * d
		}
		if dist < bestDist {
			bestDist = dist
			best = c
		}
	}
	return best
}

// minSqDistTo 返回 v 到 centers 中最近中心的欧氏距离平方。
func minSqDistTo(v []float64, centers [][]float64) float64 {
	best := math.Inf(1)
	for _, c := range centers {
		d := 0.0
		for i, x := range v {
			e := x - c[i]
			d += e * e
		}
		if d < best {
			best = d
		}
	}
	return best
}

// clusterSeed 由媒体集合的内容决定，同一批数据每次运行得到相同聚类。
func clusterSeed(items []*Media) int64 {
	sum := int64(0)
	for i, m := range items {
		for _, b := range m.Name {
			sum += int64(b)
		}
		sum += int64(len(m.ClusterTags())) * int64(131+i)
	}
	if !items[0].ModTime.IsZero() {
		sum += items[0].ModTime.Unix()
	}
	return sum
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
