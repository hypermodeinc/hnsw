package hnsw

import (
	"cmp"
	"fmt"
	"math"
	"math/rand"
	"slices"
	"sync"
	"time"

	"github.com/hypermodeinc/hnsw/heap"
	"golang.org/x/exp/maps"
)

type Vector = []float32

// Node is a node in the graph.
type Node[K cmp.Ordered] struct {
	Key   K
	Value Vector
}

func MakeNodes[K cmp.Ordered](keys []K, vecs []Vector) ([]Node[K], error) {
	if len(keys) != len(vecs) {
		return nil, fmt.Errorf("keys and vecs must have the same length")
	}

	nodes := make([]Node[K], len(keys))
	for i := range keys {
		nodes[i] = MakeNode(keys[i], vecs[i])
	}
	return nodes, nil
}

func MakeNode[K cmp.Ordered](key K, vec Vector) Node[K] {
	return Node[K]{Key: key, Value: vec}
}

// layerNode is a node in a layer of the graph.
type layerNode[K cmp.Ordered] struct {
	Node[K]

	// neighbors is map of neighbor keys to neighbor nodes.
	// It is a map and not a slice to allow for efficient deletes, esp.
	// when M is high.
	neighbors map[K]*layerNode[K]
}

// addNeighbor adds a o neighbor to the node, replacing the neighbor
// with the worst distance if the neighbor set is full.
func (n *layerNode[K]) addNeighbor(newNode *layerNode[K], m int, dist DistanceFunc) error {
	if n.neighbors == nil {
		n.neighbors = make(map[K]*layerNode[K], m)
	}

	n.neighbors[newNode.Key] = newNode
	if len(n.neighbors) <= m {
		return nil
	}

	// Find the neighbor with the worst distance.
	var (
		worstDist = float32(math.Inf(-1))
		worst     *layerNode[K]
	)
	for _, neighbor := range n.neighbors {
		d, err := dist(neighbor.Value, n.Value)
		if err != nil {
			return err
		}
		// d > worstDist may always be false if the distance function
		// returns NaN, e.g., when the embeddings are zero.
		if d > worstDist || worst == nil {
			worstDist = d
			worst = neighbor
		}
	}

	delete(n.neighbors, worst.Key)
	// Delete backlink from the worst neighbor.
	delete(worst.neighbors, n.Key)
	worst.replenish(m)

	return nil
}

type searchCandidate[K cmp.Ordered] struct {
	node *layerNode[K]
	dist float32
}

func (s searchCandidate[K]) Less(o searchCandidate[K]) bool {
	return s.dist < o.dist
}

// search returns the layer node closest to the target node
// within the same layer.
func (n *layerNode[K]) search(
	// k is the number of candidates in the result set.
	k int,
	efSearch int,
	target Vector,
	distance DistanceFunc,
) ([]searchCandidate[K], error) {
	// This is a basic greedy algorithm to find the entry point at the given level
	// that is closest to the target node.
	if n == nil {
		return nil, fmt.Errorf("node is nil")
	}
	candidates := heap.Heap[searchCandidate[K]]{}
	candidates.Init(make([]searchCandidate[K], 0, efSearch))
	dist, err := distance(n.Value, target)
	if err != nil {
		return nil, err
	}
	candidates.Push(
		searchCandidate[K]{
			node: n,
			dist: dist,
		},
	)
	var (
		result  = heap.Heap[searchCandidate[K]]{}
		visited = make(map[K]bool)
	)
	result.Init(make([]searchCandidate[K], 0, k))

	// Begin with the entry node in the result set.
	result.Push(candidates.Min())
	visited[n.Key] = true

	for candidates.Len() > 0 {
		var (
			current  = candidates.Pop().node
			improved = false
		)

		// We iterate the map in a sorted, deterministic fashion for
		// tests.
		neighborKeys := maps.Keys(current.neighbors)
		slices.Sort(neighborKeys)
		for _, neighborID := range neighborKeys {
			neighbor := current.neighbors[neighborID]
			if visited[neighborID] {
				continue
			}
			visited[neighborID] = true

			dist, err := distance(neighbor.Value, target)
			if err != nil {
				return nil, err
			}

			improved = improved || dist < result.Min().dist
			if result.Len() < k {
				result.Push(searchCandidate[K]{node: neighbor, dist: dist})
			} else if dist < result.Max().dist {
				result.PopLast()
				result.Push(searchCandidate[K]{node: neighbor, dist: dist})
			}

			candidates.Push(searchCandidate[K]{node: neighbor, dist: dist})
			// Always store candidates if we haven't reached the limit.
			if candidates.Len() > efSearch {
				candidates.PopLast()
			}
		}

		// Termination condition: no improvement in distance and at least
		// kMin candidates in the result set.
		if !improved && result.Len() >= k {
			break
		}
	}

	return result.Slice(), nil
}

func (n *layerNode[K]) replenish(m int) {
	if len(n.neighbors) >= m {
		return
	}

	// Restore connectivity by adding new neighbors.
	// This is a naive implementation that could be improved by
	// using a priority queue to find the best candidates.
	for _, neighbor := range n.neighbors {
		for key, candidate := range neighbor.neighbors {
			if _, ok := n.neighbors[key]; ok {
				// do not add duplicates
				continue
			}
			if candidate == n {
				continue
			}
			n.addNeighbor(candidate, m, CosineDistance)
			if len(n.neighbors) >= m {
				return
			}
		}
	}
}

// isolates remove the node from the graph by removing all connections
// to neighbors.
func (n *layerNode[K]) isolate(m int) {
	for _, neighbor := range n.neighbors {
		delete(neighbor.neighbors, n.Key)
		neighbor.replenish(m)
	}
}

type layer[K cmp.Ordered] struct {
	// nodes is a map of nodes IDs to nodes.
	// All nodes in a higher layer are also in the lower layers, an essential
	// property of the graph.
	//
	// nodes is exported for interop with encoding/gob.
	nodes map[K]*layerNode[K]
}

// entry returns the entry node of the layer.
// It doesn't matter which node is returned, even that the
// entry node is consistent, so we just return the first node
// in the map to avoid tracking extra state.
func (l *layer[K]) entry() *layerNode[K] {
	if l == nil {
		return nil
	}
	for _, node := range l.nodes {
		return node
	}
	return nil
}

func (l *layer[K]) size() int {
	if l == nil {
		return 0
	}
	return len(l.nodes)
}

// Graph is a Hierarchical Navigable Small World graph.
// All public parameters must be set before adding nodes to the graph.
// K is cmp.Ordered instead of of comparable so that they can be sorted.
type Graph[K cmp.Ordered] struct {
	mu sync.RWMutex
	// Distance is the distance function used to compare embeddings.
	Distance DistanceFunc

	// Rng is used for level generation. It may be set to a deterministic value
	// for reproducibility. Note that deterministic number generation can lead to
	// degenerate graphs when exposed to adversarial inputs.
	Rng *rand.Rand

	// M is the maximum number of neighbors to keep for each node.
	// A good default for OpenAI embeddings is 16.
	M int

	// Ml is the level generation factor.
	// E.g., for Ml = 0.25, each layer is 1/4 the size of the previous layer.
	Ml float64

	// EfSearch is the number of nodes to consider in the search phase.
	// 20 is a reasonable default. Higher values improve search accuracy at
	// the expense of memory.
	EfSearch int

	// EfConstruction is the number of nodes to consider in the construction phase.
	// 16 is a reasonable default. Higher values improve graph quality at the
	// expense of memory.
	EfConstruction int

	// layers is a slice of layers in the graph.
	layers []*layer[K]
}

func defaultRand() *rand.Rand {
	return rand.New(rand.NewSource(time.Now().UnixNano()))
}

// NewGraph returns a new graph with default parameters, roughly designed for
// storing OpenAI embeddings.
func NewGraph[K cmp.Ordered]() *Graph[K] {
	return &Graph[K]{
		M:              16,
		Ml:             0.25,
		Distance:       CosineDistance,
		EfSearch:       20,
		EfConstruction: 40,
		Rng:            defaultRand(),
	}
}

// maxLevel returns an upper-bound on the number of levels in the graph
// based on the size of the base layer.
func maxLevel(ml float64, numNodes int) (int, error) {
	if ml == 0 {
		return 0, fmt.Errorf("ml must be greater than 0")
	}

	if numNodes == 0 {
		return 1, nil
	}

	l := math.Log(float64(numNodes))
	l /= math.Log(1 / ml)

	m := int(math.Round(l)) + 1

	return m, nil
}

// randomLevel generates a random level for a new node.
func (h *Graph[K]) randomLevel() (int, error) {
	// max avoids having to accept an additional parameter for the maximum level
	// by calculating a probably good one from the size of the base layer.
	max := 1
	if len(h.layers) > 0 {
		if h.Ml == 0 {
			return 0, fmt.Errorf("(*Graph).Ml must be greater than 0")
		}
		var err error
		max, err = maxLevel(h.Ml, h.layers[0].size())
		if err != nil {
			return 0, err
		}
	}

	for level := 0; level < max; level++ {
		if h.Rng == nil {
			h.Rng = defaultRand()
		}
		r := h.Rng.Float64()
		if r > h.Ml {
			return level, nil
		}
	}

	return max, nil
}

func (g *Graph[K]) assertDims(n Vector) error {
	if len(g.layers) == 0 {
		return nil
	}
	dims := g.Dims()
	if dims != len(n) {
		return fmt.Errorf("embedding dimension mismatch: %d != %d", dims, len(n))
	}
	return nil
}

// Dims returns the number of dimensions in the graph, or
// 0 if the graph is empty.
func (g *Graph[K]) Dims() int {
	if len(g.layers) == 0 {
		return 0
	}
	return len(g.layers[0].entry().Value)
}

func ptr[T any](v T) *T {
	return &v
}

// Add inserts nodes into the graph.
// If another node with the same ID exists, it is replaced.
func (g *Graph[K]) Add(nodes ...Node[K]) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, node := range nodes {
		wasUpdated := false
		key := node.Key
		vec := node.Value

		g.assertDims(vec)
		insertLevel, err := g.randomLevel()
		if err != nil {
			return err
		}
		// Create layers that don't exist yet.
		for insertLevel >= len(g.layers) {
			g.layers = append(g.layers, &layer[K]{})
		}

		if insertLevel < 0 {
			return fmt.Errorf("invalid level: %d", insertLevel)
		}

		var elevator *K

		preLen := g.Len()

		// Insert node at each layer, beginning with the highest.
		for i := len(g.layers) - 1; i >= 0; i-- {
			layer := g.layers[i]
			newNode := &layerNode[K]{
				Node: Node[K]{
					Key:   key,
					Value: vec,
				},
			}

			// Insert the new node into the layer.
			if layer.entry() == nil {
				layer.nodes = map[K]*layerNode[K]{key: newNode}
				continue
			}

			// Now at the highest layer with more than one node, so we can begin
			// searching for the best way to enter the graph.
			searchPoint := layer.entry()

			// On subsequent layers, we use the elevator node to enter the graph
			// at the best point.
			if elevator != nil {
				searchPoint = layer.nodes[*elevator]
			}

			if g.Distance == nil {
				return fmt.Errorf("(*Graph).Distance must be set")
			}

			neighborhood, err := searchPoint.search(g.M, g.EfConstruction, vec, g.Distance)
			if err != nil {
				return err
			}
			if len(neighborhood) == 0 {
				// This should never happen because the searchPoint itself
				// should be in the result set.
				return fmt.Errorf("empty neighborhood")
			}

			// Re-set the elevator node for the next layer.
			elevator = ptr(neighborhood[0].node.Key)

			if insertLevel >= i {
				if node, ok := layer.nodes[key]; ok {
					delete(layer.nodes, key)
					node.isolate(g.M)
					wasUpdated = true
				}
				// Insert the new node into the layer.
				layer.nodes[key] = newNode
				for _, node := range neighborhood {
					// Create a bi-directional edge between the new node and the best node.
					node.node.addNeighbor(newNode, g.M, g.Distance)
					newNode.addNeighbor(node.node, g.M, g.Distance)
				}
			}
		}

		// Invariant check: the node should have been added to the graph.
		if wasUpdated {
			if g.Len() != preLen {
				return fmt.Errorf("node not updated")
			}
		} else {
			if g.Len() != preLen+1 {
				return fmt.Errorf("node not added")
			}
		}
	}
	return nil
}

type SearchResultNode[K cmp.Ordered] struct {
	Node[K]
	Distance float32
}

// Search finds the k nearest neighbors from the target node.
func (h *Graph[K]) Search(near Vector, k int) ([]SearchResultNode[K], error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	h.assertDims(near)
	if len(h.layers) == 0 {
		return nil, fmt.Errorf("graph is empty")
	}

	var (
		efSearch = h.EfSearch

		elevator *K
	)

	for layer := len(h.layers) - 1; layer >= 0; layer-- {
		searchPoint := h.layers[layer].entry()
		if elevator != nil {
			searchPoint = h.layers[layer].nodes[*elevator]
		}

		// Descending hierarchies
		if layer > 0 {
			nodes, err := searchPoint.search(1, efSearch, near, h.Distance)
			if err != nil {
				return nil, err
			}
			elevator = ptr(nodes[0].node.Key)
			continue
		}

		nodes, err := searchPoint.search(k, efSearch, near, h.Distance)
		if err != nil {
			return nil, err
		}
		out := make([]SearchResultNode[K], 0, len(nodes))

		for _, node := range nodes {
			resNode := SearchResultNode[K]{
				Node:     node.node.Node,
				Distance: node.dist,
			}
			out = append(out, resNode)
		}

		return out, nil
	}

	return nil, fmt.Errorf("unreachable")
}

// Len returns the number of nodes in the graph.
func (h *Graph[K]) Len() int {
	if len(h.layers) == 0 {
		return 0
	}
	return h.layers[0].size()
}

// Delete removes a node from the graph by key.
// It tries to preserve the clustering properties of the graph by
// replenishing connectivity in the affected neighborhoods.
func (h *Graph[K]) Delete(key K) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.DeleteWithLock(key)
}

func (h *Graph[K]) DeleteWithLock(key K) bool {
	if len(h.layers) == 0 {
		return false
	}

	var deleted bool
	for _, layer := range h.layers {
		node, ok := layer.nodes[key]
		if !ok {
			continue
		}
		delete(layer.nodes, key)
		node.isolate(h.M)
		deleted = true
	}

	return deleted
}

// Lookup returns the vector with the given key.
func (h *Graph[K]) Lookup(key K) (Vector, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.layers) == 0 {
		return nil, false
	}

	node, ok := h.layers[0].nodes[key]
	if !ok {
		return nil, false
	}
	return node.Value, ok
}
