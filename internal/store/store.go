package store

import (
	"sync"
	"time"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"github.com/emirpasic/gods/trees/avltree"
	"github.com/emirpasic/gods/utils"
)

// Metrics is the client used for initializing counter and gauge metrics.
type Metrics interface {
	//NewCounter initializes a new counter metric.
	NewCounter(name string) func(delta uint64)

	//NewGauge initializes a new gauge metric.
	NewGauge(name string) func(value float64)
}

// Store is an in memory data store for envelopes. It will keep a bounded
// number and drop older data once that threshold is exceeded. All functions
// are thread safe.
type Store struct {
	mu           sync.RWMutex
	size         int
	maxPerSource int

	indexes map[string]*avltree.Tree

	// oldestValueTree stores each tree's oldest value for pruning. As data is
	// added and needs to be pruned, it is done so from here.
	oldestValueTree *treeStorage

	// count is incremented each Put. It is used to determine when to prune. When
	// an envelope is pruned, it is decremented.
	count int

	// metrics
	incExpired     func(delta uint64)
	setCachePeriod func(value float64)
	incIngress     func(delta uint64)
	incEgress      func(delta uint64)
}

// NewStore creates a new store.
func NewStore(size, maxPerSource int, m Metrics) *Store {
	return &Store{
		size:            size,
		maxPerSource:    maxPerSource,
		indexes:         make(map[string]*avltree.Tree),
		oldestValueTree: newTreeStorage(),

		incExpired:     m.NewCounter("Expired"),
		setCachePeriod: m.NewGauge("CachePeriod"),
		incIngress:     m.NewCounter("Ingress"),
		incEgress:      m.NewCounter("Egress"),
	}
}

// Put adds a batch of envelopes into the store.
func (s *Store) Put(e *loggregator_v2.Envelope, index string) {
	s.incIngress(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.indexes[index]
	if !ok {
		t = avltree.NewWith(utils.Int64Comparator)
		s.indexes[index] = t

		// Store the tree for pruning purposes.
		s.oldestValueTree.Put(e.Timestamp, t)
	}

	var (
		oldest    int64
		hasOldest bool
	)
	if t.Size() > 0 {
		oldest = t.Left().Key.(int64)
		hasOldest = true
	}

	preSize := t.Size()

	if preSize >= s.maxPerSource {
		// This index has reached/exceeded its allowed quota. Truncate the
		// oldest before putting a new envelope in.
		t.Remove(oldest)
		s.incExpired(1)
	}

	t.Put(e.Timestamp, envelopeWrapper{e: e, index: index})

	// Only increment if we didn't overwrite.
	s.count += t.Size() - preSize

	newOldest := t.Left().Key.(int64)
	if oldest != newOldest && hasOldest {
		s.oldestValueTree.Remove(oldest, t)
		s.oldestValueTree.Put(newOldest, t)
	}

	s.truncate()

	oldestValue, _ := s.oldestValueTree.Left()
	cachePeriod := (time.Now().UnixNano() - oldestValue) / int64(time.Millisecond)
	s.setCachePeriod(float64(cachePeriod))
}

// truncate removes the oldest envelope from the entire cache. It considers
// each source-id.
func (s *Store) truncate() {
	if s.count <= s.size {
		return
	}

	s.count--
	s.incExpired(1)

	// dereference the node so that after we remove it, the pointer does not
	// get updated underneath us.
	key, oldTree := s.oldestValueTree.Left()
	s.oldestValueTree.Remove(key, oldTree)

	// Truncate the oldest envelope.
	left := oldTree.Left()

	index := left.Value.(envelopeWrapper).index

	oldTree.Remove(key)

	if oldTree.Size() == 0 {
		// Remove the index completely.
		delete(s.indexes, index)
		return
	}

	// Add tree back to oldestValueTree for future pruning.
	s.oldestValueTree.Put(oldTree.Left().Key.(int64), oldTree)
}

// EnvelopeType is used to filter envelopes based on type.
type EnvelopeType interface{}

// Get fetches envelopes from the store based on the source ID, start and end
// time. Start is inclusive while end is not: [start..end).
func (s *Store) Get(
	index string,
	start time.Time,
	end time.Time,
	envelopeType EnvelopeType,
	limit int,
) []*loggregator_v2.Envelope {
	s.mu.RLock()
	defer s.mu.RUnlock()

	t, ok := s.indexes[index]
	if !ok {
		return nil
	}

	var res []*loggregator_v2.Envelope
	s.treeTraverse(t.Root, start.UnixNano(), end.UnixNano(), func(e *loggregator_v2.Envelope, idx string) bool {
		if idx == index &&
			s.checkEnvelopeType(e, envelopeType) {
			res = append(res, e)
		}

		// Return true to stop traversing
		return len(res) >= limit
	})

	s.incEgress(uint64(len(res)))
	return res
}

func (s *Store) treeTraverse(
	n *avltree.Node,
	start int64,
	end int64,
	f func(e *loggregator_v2.Envelope, index string) bool,
) bool {
	if n == nil {
		return false
	}

	t := n.Key.(int64)
	if t >= start {
		if s.treeTraverse(n.Children[0], start, end, f) {
			return true
		}

		w := n.Value.(envelopeWrapper)

		if t >= end || f(w.e, w.index) {
			return true
		}
	}

	return s.treeTraverse(n.Children[1], start, end, f)
}

func (s *Store) checkEnvelopeType(e *loggregator_v2.Envelope, t EnvelopeType) bool {
	if t == nil {
		return true
	}

	switch t.(type) {
	case *loggregator_v2.Log:
		return e.GetLog() != nil
	case *loggregator_v2.Counter:
		return e.GetCounter() != nil
	case *loggregator_v2.Gauge:
		return e.GetGauge() != nil
	case *loggregator_v2.Timer:
		return e.GetTimer() != nil
	case *loggregator_v2.Event:
		return e.GetEvent() != nil
	default:
		// This should never happen. This implies the store is being used
		// poorly.
		panic("unknown type")
	}
}

// treeStorage stores the trees and sorts them with respect to time. It
// prevents overwrites for the same key.
type treeStorage struct {
	t *avltree.Tree
}

func newTreeStorage() *treeStorage {
	return &treeStorage{
		t: avltree.NewWith(utils.Int64Comparator),
	}
}

func (s *treeStorage) Put(key int64, t *avltree.Tree) {
	var values []*avltree.Tree
	if existing, found := s.t.Get(key); found {
		values = existing.([]*avltree.Tree)
	}

	s.t.Put(key, append(values, t))
}

func (s *treeStorage) Remove(key int64, t *avltree.Tree) {
	var values []*avltree.Tree
	if existing, found := s.t.Get(key); found {
		values = existing.([]*avltree.Tree)
	}

	for i, v := range values {
		if v == t {
			values = append(values[:i], values[i+1:]...)
			break
		}
	}

	if len(values) == 0 {
		s.t.Remove(key)
		return
	}

	s.t.Put(key, values)
}

func (s *treeStorage) Left() (int64, *avltree.Tree) {
	l := s.t.Left()
	return l.Key.(int64), l.Value.([]*avltree.Tree)[0]
}

type envelopeWrapper struct {
	e     *loggregator_v2.Envelope
	index string
}
