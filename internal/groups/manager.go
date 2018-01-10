package groups

import (
	"context"
	"sync"
	"time"

	"code.cloudfoundry.org/go-log-cache/rpc/logcache"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// Manager manages groups. It implements logcache.GroupReader.
type Manager struct {
	mu               sync.RWMutex
	m                map[string]groupInfo
	s                DataStorage
	requesterTimeout time.Duration
}

// DataStorage is used to store data for a given group.
type DataStorage interface {
	// Get fetches envelopes from the store based on the source ID, start and
	// end time. Start is inclusive while end is not: [start..end).
	Get(
		name string,
		start time.Time,
		end time.Time,
		envelopeType store.EnvelopeType,
		limit int,
		requesterID uint64,
	) []*loggregator_v2.Envelope

	// Add starts fetching data for the given sourceID.
	Add(name, sourceID string)

	// AddRequester adds a requester ID for a given group.
	AddRequester(name string, requesterID uint64)

	// Remove stops fetching data for the given sourceID.
	Remove(name, sourceID string)

	// RemoveRequester removes a requester ID for a given group.
	RemoveRequester(name string, requesterID uint64)
}

// NewManager creates a new Manager to manage groups.
func NewManager(s DataStorage, requesterTimeout time.Duration) *Manager {
	return &Manager{
		m:                make(map[string]groupInfo),
		s:                s,
		requesterTimeout: requesterTimeout,
	}
}

// AddToGroup creates the given group if it does not exist or adds the
// sourceID if it does.
func (m *Manager) AddToGroup(ctx context.Context, r *logcache.AddToGroupRequest, _ ...grpc.CallOption) (*logcache.AddToGroupResponse, error) {
	if r.GetName() == "" || r.GetSourceId() == "" {
		return nil, grpc.Errorf(codes.InvalidArgument, "name and source_id fields are required")
	}

	if len(r.GetName()) > 128 || len(r.GetSourceId()) > 128 {
		return nil, grpc.Errorf(codes.InvalidArgument, "name and source_id fields can only be 128 bytes long")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	gi, ok := m.m[r.Name]
	if !ok {
		gi.requesterIDs = make(map[uint64]time.Time)
	}

	gi.sourceIDs = append(gi.sourceIDs, r.SourceId)
	m.m[r.Name] = gi
	m.s.Add(r.Name, r.SourceId)

	return &logcache.AddToGroupResponse{}, nil
}

// RemoveFromGroup removes a source ID from the given group. If that was the
// last entry, then the group is removed. If the group already didn't exist,
// then it is a nop.
func (m *Manager) RemoveFromGroup(ctx context.Context, r *logcache.RemoveFromGroupRequest, _ ...grpc.CallOption) (*logcache.RemoveFromGroupResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.m[r.Name]
	if !ok {
		return &logcache.RemoveFromGroupResponse{}, nil
	}

	for i, x := range a.sourceIDs {
		if x == r.SourceId {
			a.sourceIDs = append(a.sourceIDs[:i], a.sourceIDs[i+1:]...)
			m.s.Remove(r.Name, r.SourceId)
			break
		}
	}
	m.m[r.Name] = a

	if len(m.m[r.Name].sourceIDs) == 0 {
		delete(m.m, r.Name)
	}
	return &logcache.RemoveFromGroupResponse{}, nil
}

func (m *Manager) Read(ctx context.Context, r *logcache.GroupReadRequest, _ ...grpc.CallOption) (*logcache.GroupReadResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	gi, ok := m.m[r.Name]
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, "unknown group name: %s", r.GetName())
	}

	if _, ok := gi.requesterIDs[r.RequesterId]; !ok {
		m.s.AddRequester(r.Name, r.RequesterId)
	}
	gi.requesterIDs[r.RequesterId] = time.Now()

	// Check for expired requesters
	for k, v := range m.m[r.Name].requesterIDs {
		if time.Since(v) >= m.requesterTimeout {
			delete(m.m[r.Name].requesterIDs, k)
			m.s.RemoveRequester(r.Name, k)
		}
	}

	if r.GetEndTime() == 0 {
		r.EndTime = time.Now().UnixNano()
	}

	if r.GetLimit() == 0 {
		r.Limit = 100
	}

	batch := m.s.Get(
		r.GetName(),
		time.Unix(0, r.GetStartTime()),
		time.Unix(0, r.GetEndTime()),
		m.convertEnvelopeType(r.GetEnvelopeType()),
		int(r.GetLimit()),
		r.RequesterId,
	)

	return &logcache.GroupReadResponse{
		Envelopes: &loggregator_v2.EnvelopeBatch{
			Batch: batch,
		},
	}, nil
}

// Group returns information about the given group. If the group does not
// exist, the returned sourceID slice will be empty, but an error will not be
// returned.
func (m *Manager) Group(ctx context.Context, r *logcache.GroupRequest, _ ...grpc.CallOption) (*logcache.GroupResponse, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.m[r.Name]

	var reqIds []uint64
	for k, _ := range a.requesterIDs {
		reqIds = append(reqIds, k)
	}

	return &logcache.GroupResponse{
		SourceIds:    a.sourceIDs,
		RequesterIds: reqIds,
	}, nil
}

func (m *Manager) convertEnvelopeType(t logcache.EnvelopeTypes) store.EnvelopeType {
	switch t {
	case logcache.EnvelopeTypes_LOG:
		return &loggregator_v2.Log{}
	case logcache.EnvelopeTypes_COUNTER:
		return &loggregator_v2.Counter{}
	case logcache.EnvelopeTypes_GAUGE:
		return &loggregator_v2.Gauge{}
	case logcache.EnvelopeTypes_TIMER:
		return &loggregator_v2.Timer{}
	case logcache.EnvelopeTypes_EVENT:
		return &loggregator_v2.Event{}
	default:
		return nil
	}
}

type groupInfo struct {
	sourceIDs    []string
	requesterIDs map[uint64]time.Time
}
