package logcache_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	rpc "code.cloudfoundry.org/go-log-cache/rpc/logcache_v1"
	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
	"code.cloudfoundry.org/log-cache"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("LogCache", func() {
	var (
		tlsConfig *tls.Config
		peer      *spyLogCache
		cache     *logcache.LogCache
		oc        rpc.OrchestrationClient

		spyMetrics *spyMetrics
		run        int
	)

	BeforeEach(func() {
		var err error
		tlsConfig, err = newTLSConfig(
			Cert("log-cache-ca.crt"),
			Cert("log-cache.crt"),
			Cert("log-cache.key"),
			"log-cache",
		)
		Expect(err).ToNot(HaveOccurred())

		peer = newSpyLogCache(tlsConfig)
		peerAddr := peer.start()
		spyMetrics = newSpyMetrics()

		addr := fmt.Sprintf("127.0.0.1:%d", 9000+run)

		cache = logcache.New(
			logcache.WithAddr(addr),
			logcache.WithClustered(0, []string{addr, peerAddr},
				grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
			),
			logcache.WithMetrics(spyMetrics),
			logcache.WithServerOpts(
				grpc.Creds(credentials.NewTLS(tlsConfig)),
			),
		)
		cache.Start()

		conn, err := grpc.Dial(
			cache.Addr(),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		)
		Expect(err).ToNot(HaveOccurred())

		oc = rpc.NewOrchestrationClient(conn)

		_, err = oc.SetRanges(context.Background(), &rpc.SetRangesRequest{
			Ranges: map[string]*rpc.Ranges{
				cache.Addr(): {
					Ranges: []*rpc.Range{
						{
							Start: 0,
							End:   9223372036854775807,
						},
					},
				},
				peerAddr: {
					Ranges: []*rpc.Range{
						{
							Start: 9223372036854775808,
							End:   math.MaxUint64,
						},
					},
				},
			},
		})
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		run++
		cache.Close()
	})

	It("returns tail of data filtered by source ID", func() {
		go func() {
			addr := cache.Addr()
			for range time.Tick(time.Millisecond) {
				writeEnvelopes(addr, []*loggregator_v2.Envelope{
					// source-0 hashes to 7700738999732113484 (route to node 0)
					{Timestamp: 1, SourceId: "source-0"},
					// source-1 hashes to 15704273932878139171 (route to node 1)
					{Timestamp: 2, SourceId: "source-1"},
					{Timestamp: 3, SourceId: "source-0"},
					{Timestamp: 4, SourceId: "source-0"},
				})
			}
		}()

		conn, err := grpc.Dial(cache.Addr(),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		)
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		client := rpc.NewEgressClient(conn)

		var es []*loggregator_v2.Envelope
		f := func() error {
			resp, err := client.Read(context.Background(), &rpc.ReadRequest{
				SourceId:   "source-0",
				Descending: true,
				Limit:      2,
			})
			if err != nil {
				return err
			}

			if len(resp.Envelopes.Batch) != 2 {
				return errors.New("expected 2 envelopes")
			}

			es = resp.Envelopes.Batch
			return nil
		}
		Eventually(f, 5).Should(BeNil())

		Expect(es[0].Timestamp).To(Equal(int64(4)))
		Expect(es[0].SourceId).To(Equal("source-0"))
		Expect(es[1].Timestamp).To(Equal(int64(3)))
		Expect(es[1].SourceId).To(Equal("source-0"))

		Eventually(spyMetrics.getter("Ingress")).Should(BeNumerically(">=", 3))
		Eventually(spyMetrics.getter("Egress")).Should(BeNumerically(">=", 2))
	})

	It("uses the routes from the scheduler", func() {
		_, err := oc.SetRanges(context.Background(), &rpc.SetRangesRequest{
			Ranges: map[string]*rpc.Ranges{
				cache.Addr(): {
					Ranges: []*rpc.Range{
						{
							Start: 0,
							End:   math.MaxUint64,
						},
					},
				},
			},
		})
		Expect(err).ToNot(HaveOccurred())

		Eventually(func() uint64 {
			writeEnvelopes(cache.Addr(), []*loggregator_v2.Envelope{
				{Timestamp: 1, SourceId: "source-0"},
				{Timestamp: 2, SourceId: "source-1"},
				{Timestamp: 3, SourceId: "source-0"},
				{Timestamp: 4, SourceId: "source-0"},
			})

			return spyMetrics.getter("Ingress")()
		}, 5).Should(BeNumerically(">=", 4))

		Expect(peer.getEnvelopes()).To(BeEmpty())
	})

	It("routes envelopes to peers", func() {
		writeEnvelopes(cache.Addr(), []*loggregator_v2.Envelope{
			// source-0 hashes to 7700738999732113484 (route to node 0)
			{Timestamp: 1, SourceId: "source-0"},
			// source-1 hashes to 15704273932878139171 (route to node 1)
			{Timestamp: 2, SourceId: "source-1"},
			{Timestamp: 3, SourceId: "source-1"},
		})

		Eventually(func() int {
			return len(peer.getEnvelopes())
		}).Should(BeNumerically(">=", 2))

		Expect(peer.getEnvelopes()).To(ContainElement(&loggregator_v2.Envelope{Timestamp: 2, SourceId: "source-1"}))
		Expect(peer.getEnvelopes()).To(ContainElement(&loggregator_v2.Envelope{Timestamp: 3, SourceId: "source-1"}))
		Expect(peer.getLocalOnlyValues()).ToNot(ContainElement(false))
	})

	It("accepts envelopes from peers", func() {
		conn, err := grpc.Dial(cache.Addr(),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		)
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		client := rpc.NewEgressClient(conn)

		var es []*loggregator_v2.Envelope
		f := func() error {
			// source-0 hashes to 7700738999732113484 (route to node 0)
			writeEnvelopes(cache.Addr(), []*loggregator_v2.Envelope{
				{SourceId: "source-0", Timestamp: 1},
			})

			resp, err := client.Read(context.Background(), &rpc.ReadRequest{
				SourceId: "source-0",
			})
			if err != nil {
				return err
			}

			if len(resp.Envelopes.Batch) == 0 {
				return errors.New("expected atleast 1 envelope")
			}

			es = resp.Envelopes.Batch
			return nil
		}
		Eventually(f, 5).Should(BeNil())

		Expect(es[0].Timestamp).To(Equal(int64(1)))
		Expect(es[0].SourceId).To(Equal("source-0"))
	})

	It("routes query requests to peers", func() {
		peer.readEnvelopes["source-1"] = func() []*loggregator_v2.Envelope {
			return []*loggregator_v2.Envelope{
				{Timestamp: 99},
				{Timestamp: 101},
			}
		}

		conn, err := grpc.Dial(cache.Addr(),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		)
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		client := rpc.NewEgressClient(conn)

		// source-1 hashes to 15704273932878139171 (route to node 1)
		resp, err := client.Read(context.Background(), &rpc.ReadRequest{
			SourceId:      "source-1",
			StartTime:     99,
			EndTime:       101,
			EnvelopeTypes: []rpc.EnvelopeType{rpc.EnvelopeType_LOG},
		})
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.Envelopes.Batch).To(HaveLen(2))

		Eventually(peer.getReadRequests).Should(HaveLen(1))
		req := peer.getReadRequests()[0]
		Expect(req.SourceId).To(Equal("source-1"))
		Expect(req.StartTime).To(Equal(int64(99)))
		Expect(req.EndTime).To(Equal(int64(101)))
		Expect(req.EnvelopeTypes).To(ConsistOf(rpc.EnvelopeType_LOG))
	})

	It("returns all meta information", func() {
		peer.metaResponses = map[string]*rpc.MetaInfo{
			"source-1": {
				Count:           1,
				Expired:         2,
				OldestTimestamp: 3,
				NewestTimestamp: 4,
			},
		}

		conn, err := grpc.Dial(cache.Addr(),
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		)
		Expect(err).ToNot(HaveOccurred())
		defer conn.Close()
		ingressClient := rpc.NewIngressClient(conn)
		egressClient := rpc.NewEgressClient(conn)

		Eventually(func() map[string]*rpc.MetaInfo {
			sendRequest := &rpc.SendRequest{
				Envelopes: &loggregator_v2.EnvelopeBatch{
					Batch: []*loggregator_v2.Envelope{
						{SourceId: "source-0"},
					},
				},
			}

			ingressClient.Send(context.Background(), sendRequest)
			resp, err := egressClient.Meta(context.Background(), &rpc.MetaRequest{})
			if err != nil {
				return nil
			}

			return resp.Meta
		}, 5).Should(And(
			HaveKey("source-0"),
			HaveKeyWithValue("source-1", &rpc.MetaInfo{
				Count:           1,
				Expired:         2,
				OldestTimestamp: 3,
				NewestTimestamp: 4,
			}),
		))
	})
})

func writeEnvelopes(addr string, es []*loggregator_v2.Envelope) {
	tlsConfig, err := newTLSConfig(
		Cert("log-cache-ca.crt"),
		Cert("log-cache.crt"),
		Cert("log-cache.key"),
		"log-cache",
	)
	if err != nil {
		panic(err)
	}
	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
	)
	if err != nil {
		panic(err)
	}

	client := rpc.NewIngressClient(conn)
	var envelopes []*loggregator_v2.Envelope
	for _, e := range es {
		envelopes = append(envelopes, &loggregator_v2.Envelope{
			Timestamp: e.Timestamp,
			SourceId:  e.SourceId,
		})
	}

	client.Send(context.Background(), &rpc.SendRequest{
		Envelopes: &loggregator_v2.EnvelopeBatch{
			Batch: envelopes,
		},
	})
}

type spyLogCache struct {
	mu              sync.Mutex
	localOnlyValues []bool
	envelopes       []*loggregator_v2.Envelope
	readRequests    []*rpc.ReadRequest
	readEnvelopes   map[string]func() []*loggregator_v2.Envelope
	metaResponses   map[string]*rpc.MetaInfo
	tlsConfig       *tls.Config
}

func newSpyLogCache(tlsConfig *tls.Config) *spyLogCache {
	return &spyLogCache{
		readEnvelopes: make(map[string]func() []*loggregator_v2.Envelope),
		tlsConfig:     tlsConfig,
	}
}

func (s *spyLogCache) start() string {
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(s.tlsConfig)),
	)
	rpc.RegisterIngressServer(srv, s)
	rpc.RegisterEgressServer(srv, s)
	go srv.Serve(lis)

	return lis.Addr().String()
}

func (s *spyLogCache) getEnvelopes() []*loggregator_v2.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := make([]*loggregator_v2.Envelope, len(s.envelopes))
	copy(r, s.envelopes)
	return r
}

func (s *spyLogCache) getLocalOnlyValues() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := make([]bool, len(s.localOnlyValues))
	copy(r, s.localOnlyValues)
	return r
}

func (s *spyLogCache) getReadRequests() []*rpc.ReadRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := make([]*rpc.ReadRequest, len(s.readRequests))
	copy(r, s.readRequests)
	return r
}

func (s *spyLogCache) Send(ctx context.Context, r *rpc.SendRequest) (*rpc.SendResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.localOnlyValues = append(s.localOnlyValues, r.LocalOnly)

	for _, e := range r.Envelopes.Batch {
		s.envelopes = append(s.envelopes, e)
	}

	return &rpc.SendResponse{}, nil
}

func (s *spyLogCache) Read(ctx context.Context, r *rpc.ReadRequest) (*rpc.ReadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.readRequests = append(s.readRequests, r)

	b := s.readEnvelopes[r.GetSourceId()]

	var batch []*loggregator_v2.Envelope
	if b != nil {
		batch = b()
	}

	return &rpc.ReadResponse{
		Envelopes: &loggregator_v2.EnvelopeBatch{
			Batch: batch,
		},
	}, nil
}

func (s *spyLogCache) Meta(ctx context.Context, r *rpc.MetaRequest) (*rpc.MetaResponse, error) {
	return &rpc.MetaResponse{
		Meta: s.metaResponses,
	}, nil
}

func newTLSConfig(caPath, certPath, keyPath, cn string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		ServerName:         cn,
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: false,
	}

	caCertBytes, err := ioutil.ReadFile(caPath)
	if err != nil {
		return nil, err
	}

	caCertPool := x509.NewCertPool()
	if ok := caCertPool.AppendCertsFromPEM(caCertBytes); !ok {
		return nil, errors.New("cannot parse ca cert")
	}

	tlsConfig.RootCAs = caCertPool

	return tlsConfig, nil
}
