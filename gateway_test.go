package logcache_test

import (
	"fmt"
	"net/http"

	rpc "code.cloudfoundry.org/go-log-cache/rpc/logcache_v1"
	"code.cloudfoundry.org/log-cache"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
)

var _ = Describe("Gateway", func() {
	var (
		spyLogCache    *spyLogCache
		spyGroupReader *spyGroupReader

		gw *logcache.Gateway
	)

	BeforeEach(func() {
		tlsConfig, err := newTLSConfig(
			Cert("log-cache-ca.crt"),
			Cert("log-cache.crt"),
			Cert("log-cache.key"),
			"log-cache",
		)
		Expect(err).ToNot(HaveOccurred())

		spyLogCache = newSpyLogCache(tlsConfig)
		logCacheAddr := spyLogCache.start()

		spyGroupReader = newSpyGroupReader(tlsConfig)
		groupReaderAddr := spyGroupReader.start()

		gw = logcache.NewGateway(
			logCacheAddr,
			groupReaderAddr,
			"127.0.0.1:0",
			logcache.WithGatewayLogCacheDialOpts(
				grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
			),
			logcache.WithGatewayGroupReaderDialOpts(
				grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
			),
		)
		gw.Start()
	})

	DescribeTable("upgrades HTTP requests for LogCache into gRPC requests", func(pathSourceID, expectedSourceID string) {
		path := fmt.Sprintf("v1/read/%s?start_time=99&end_time=101&limit=103&envelope_types=LOG&envelope_types=GAUGE", pathSourceID)
		URL := fmt.Sprintf("http://%s/%s", gw.Addr(), path)
		resp, err := http.Get(URL)
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		reqs := spyLogCache.getReadRequests()
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].SourceId).To(Equal(expectedSourceID))
		Expect(reqs[0].StartTime).To(Equal(int64(99)))
		Expect(reqs[0].EndTime).To(Equal(int64(101)))
		Expect(reqs[0].Limit).To(Equal(int64(103)))
		Expect(reqs[0].EnvelopeTypes).To(ConsistOf(rpc.EnvelopeType_LOG, rpc.EnvelopeType_GAUGE))
	},
		Entry("URL encoded", "some-source%2Fid", "some-source/id"),
		Entry("with slash", "some-source/id", "some-source/id"),
		Entry("with dash", "some-source-id", "some-source-id"),
	)

	It("upgrades HTTP requests for GroupReader into gRPC requests", func() {
		path := "v1/group/some-name?start_time=99&end_time=101&limit=103&envelope_types=LOG"
		URL := fmt.Sprintf("http://%s/%s", gw.Addr(), path)
		resp, err := http.Get(URL)
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		reqs := spyGroupReader.getReadRequests()
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal("some-name"))
		Expect(reqs[0].StartTime).To(Equal(int64(99)))
		Expect(reqs[0].EndTime).To(Equal(int64(101)))
		Expect(reqs[0].Limit).To(Equal(int64(103)))
		Expect(reqs[0].EnvelopeTypes).To(ConsistOf(rpc.EnvelopeType_LOG))
	})

	It("upgrades HTTP requests for GroupReader PUTs into gRPC requests", func() {
		path := "v1/group/some-name/some-source/id"
		URL := fmt.Sprintf("http://%s/%s", gw.Addr(), path)
		req, _ := http.NewRequest("PUT", URL, nil)

		resp, err := http.DefaultClient.Do(req)
		Expect(err).ToNot(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK))

		reqs := spyGroupReader.AddRequests()
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].Name).To(Equal("some-name"))
		Expect(reqs[0].SourceId).To(Equal("some-source/id"))
	})
})
