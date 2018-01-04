package groups_test

import (
	"log"
	"sort"
	"sync"
	"time"

	gologcache "code.cloudfoundry.org/go-log-cache"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache/internal/groups"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Storage", func() {
	var (
		s          *groups.Storage
		spyMetrics *spyMetrics
		spyReader  *spyReader
	)

	BeforeEach(func() {
		spyMetrics = newSpyMetrics()
		spyReader = newSpyReader()
		s = groups.NewStorage(5, spyReader.Read, spyMetrics, log.New(GinkgoWriter, "", 0))
	})

	It("returns data sorted by timestamp", func() {
		s.Add("some-name", "a")
		s.Add("some-name", "b")

		spyReader.addEnvelopes("a", []*loggregator_v2.Envelope{
			{Timestamp: 99, SourceId: "a"},
			{Timestamp: 101, SourceId: "a"},
			{Timestamp: 103, SourceId: "a"},
		})

		spyReader.addEnvelopes("b", []*loggregator_v2.Envelope{
			{Timestamp: 100, SourceId: "b"},
			{Timestamp: 102, SourceId: "b"},
			{Timestamp: 104, SourceId: "b"},
		})

		var ts []int64

		Eventually(func() []string {
			var r []string
			// [100, 104)
			for _, e := range s.Get("some-name", time.Unix(0, 100), time.Unix(0, 104), nil, 100) {
				r = append(r, e.GetSourceId())
				ts = append(ts, e.GetTimestamp())
			}
			return r
		}).Should(And(ContainElement("a"), ContainElement("b")))

		Expect(sort.IsSorted(int64s(ts))).To(BeTrue())

		// [100, 104)
		Expect(ts).To(HaveLen(4))
	})

	It("removes producer", func() {
		s.Add("some-name", "a")
		s.Remove("some-name", "a")

		spyReader.addEnvelopes("a", []*loggregator_v2.Envelope{
			{Timestamp: 99, SourceId: "a"},
		})

		f := func() []*loggregator_v2.Envelope {
			return s.Get("some-name", time.Unix(0, 100), time.Unix(0, 101), nil, 100)
		}()

		Eventually(f).Should(BeEmpty())
		Consistently(f).Should(BeEmpty())
	})
})

type spyReader struct {
	mu        sync.Mutex
	sourceIDs []string
	results   map[string][][]*loggregator_v2.Envelope
}

func newSpyReader() *spyReader {
	return &spyReader{
		results: make(map[string][][]*loggregator_v2.Envelope),
	}
}

func (s *spyReader) addEnvelopes(sourceID string, es []*loggregator_v2.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.results[sourceID] = append(s.results[sourceID], es)
}

func (s *spyReader) Read(sourceID string, start time.Time, opts ...gologcache.ReadOption) ([]*loggregator_v2.Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sourceIDs = append(s.sourceIDs, sourceID)

	r := s.results[sourceID]
	if len(r) == 0 {
		return nil, nil
	}

	s.results[sourceID] = r[1:]

	return r[0], nil
}

func (s *spyReader) SourceIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := make([]string, len(s.sourceIDs))
	copy(r, s.sourceIDs)
	return r
}

type int64s []int64

func (i int64s) Len() int {
	return len(i)
}

func (i int64s) Less(a, b int) bool {
	return i[a] < i[b]
}

func (i int64s) Swap(a, b int) {
	tmp := i[a]
	i[a] = i[b]
	i[b] = tmp
}

type spyMetrics struct {
	values map[string]float64
}

func newSpyMetrics() *spyMetrics {
	return &spyMetrics{
		values: make(map[string]float64),
	}
}

func (s *spyMetrics) NewCounter(name string) func(delta uint64) {
	return func(d uint64) {
		s.values[name] += float64(d)
	}
}

func (s *spyMetrics) NewGauge(name string) func(value float64) {
	return func(v float64) {
		s.values[name] = v
	}
}
