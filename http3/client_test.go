package http3

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/golang/mock/gomock"
	quic "github.com/lucas-clemente/quic-go"
	mockquic "github.com/lucas-clemente/quic-go/internal/mocks/quic"
	"github.com/lucas-clemente/quic-go/internal/utils"
	"github.com/marten-seemann/qpack"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Client", func() {
	var (
		client       *client
		req          *http.Request
		origDialAddr = dialAddr
	)

	BeforeEach(func() {
		origDialAddr = dialAddr
		hostname := "quic.clemente.io:1337"
		client = newClient(hostname, nil, &roundTripperOpts{}, nil, nil)
		Expect(client.hostname).To(Equal(hostname))

		var err error
		req, err = http.NewRequest("GET", "https://localhost:1337", nil)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		dialAddr = origDialAddr
	})

	It("uses the default QUIC config if none is give", func() {
		client = newClient("localhost:1337", nil, &roundTripperOpts{}, nil, nil)
		var dialAddrCalled bool
		dialAddr = func(_ string, _ *tls.Config, quicConf *quic.Config) (quic.Session, error) {
			Expect(quicConf).To(Equal(defaultQuicConfig))
			dialAddrCalled = true
			return nil, errors.New("test done")
		}
		client.RoundTrip(req)
		Expect(dialAddrCalled).To(BeTrue())
	})

	It("adds the port to the hostname, if none is given", func() {
		client = newClient("quic.clemente.io", nil, &roundTripperOpts{}, nil, nil)
		var dialAddrCalled bool
		dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.Session, error) {
			Expect(hostname).To(Equal("quic.clemente.io:443"))
			dialAddrCalled = true
			return nil, errors.New("test done")
		}
		req, err := http.NewRequest("GET", "https://quic.clemente.io:443", nil)
		Expect(err).ToNot(HaveOccurred())
		client.RoundTrip(req)
		Expect(dialAddrCalled).To(BeTrue())
	})

	It("uses the TLS config and QUIC config", func() {
		tlsConf := &tls.Config{ServerName: "foo.bar"}
		quicConf := &quic.Config{IdleTimeout: time.Nanosecond}
		client = newClient("localhost:1337", tlsConf, &roundTripperOpts{}, quicConf, nil)
		var dialAddrCalled bool
		dialAddr = func(
			hostname string,
			tlsConfP *tls.Config,
			quicConfP *quic.Config,
		) (quic.Session, error) {
			Expect(hostname).To(Equal("localhost:1337"))
			Expect(tlsConfP).To(Equal(tlsConf))
			Expect(quicConfP.IdleTimeout).To(Equal(quicConf.IdleTimeout))
			dialAddrCalled = true
			return nil, errors.New("test done")
		}
		client.RoundTrip(req)
		Expect(dialAddrCalled).To(BeTrue())
	})

	It("uses the custom dialer, if provided", func() {
		testErr := errors.New("test done")
		tlsConf := &tls.Config{ServerName: "foo.bar"}
		quicConf := &quic.Config{IdleTimeout: 1337 * time.Second}
		var dialerCalled bool
		dialer := func(network, address string, tlsConfP *tls.Config, quicConfP *quic.Config) (quic.Session, error) {
			Expect(network).To(Equal("udp"))
			Expect(address).To(Equal("localhost:1337"))
			Expect(tlsConfP).To(Equal(tlsConf))
			Expect(quicConfP.IdleTimeout).To(Equal(quicConf.IdleTimeout))
			dialerCalled = true
			return nil, testErr
		}
		client = newClient("localhost:1337", tlsConf, &roundTripperOpts{}, quicConf, dialer)
		_, err := client.RoundTrip(req)
		Expect(err).To(MatchError(testErr))
		Expect(dialerCalled).To(BeTrue())
	})

	It("errors when dialing fails", func() {
		testErr := errors.New("handshake error")
		client = newClient("localhost:1337", nil, &roundTripperOpts{}, nil, nil)
		dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.Session, error) {
			return nil, testErr
		}
		_, err := client.RoundTrip(req)
		Expect(err).To(MatchError(testErr))
	})

	It("errors if it can't open a stream", func() {
		testErr := errors.New("stream open error")
		client = newClient("localhost:1337", nil, &roundTripperOpts{}, nil, nil)
		session := mockquic.NewMockSession(mockCtrl)
		session.EXPECT().OpenUniStreamSync().Return(nil, testErr).MaxTimes(1)
		session.EXPECT().OpenStreamSync().Return(nil, testErr).MaxTimes(1)
		session.EXPECT().CloseWithError(gomock.Any(), gomock.Any()).MaxTimes(1)
		dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.Session, error) {
			return session, nil
		}
		defer GinkgoRecover()
		_, err := client.RoundTrip(req)
		Expect(err).To(MatchError(testErr))
	})

	Context("Doing requests", func() {
		var (
			request *http.Request
			str     *mockquic.MockStream
			sess    *mockquic.MockSession
		)

		decodeHeader := func(str io.Reader) map[string]string {
			fields := make(map[string]string)
			decoder := qpack.NewDecoder(nil)

			frame, err := parseNextFrame(str)
			Expect(err).ToNot(HaveOccurred())
			Expect(frame).To(BeAssignableToTypeOf(&headersFrame{}))
			headersFrame := frame.(*headersFrame)
			data := make([]byte, headersFrame.Length)
			_, err = io.ReadFull(str, data)
			Expect(err).ToNot(HaveOccurred())
			hfs, err := decoder.DecodeFull(data)
			Expect(err).ToNot(HaveOccurred())
			for _, p := range hfs {
				fields[p.Name] = p.Value
			}
			return fields
		}

		BeforeEach(func() {
			controlStr := mockquic.NewMockStream(mockCtrl)
			controlStr.EXPECT().Write([]byte{0x0}).Return(1, nil).MaxTimes(1)
			controlStr.EXPECT().Write(gomock.Any()).MaxTimes(1) // SETTINGS frame
			str = mockquic.NewMockStream(mockCtrl)
			sess = mockquic.NewMockSession(mockCtrl)
			sess.EXPECT().OpenUniStreamSync().Return(controlStr, nil).MaxTimes(1)
			dialAddr = func(hostname string, _ *tls.Config, _ *quic.Config) (quic.Session, error) {
				return sess, nil
			}
			var err error
			request, err = http.NewRequest("GET", "https://quic.clemente.io:1337/file1.dat", nil)
			Expect(err).ToNot(HaveOccurred())
		})

		It("sends a request", func() {
			sess.EXPECT().OpenStreamSync().Return(str, nil)
			buf := &bytes.Buffer{}
			str.EXPECT().Write(gomock.Any()).DoAndReturn(func(p []byte) (int, error) {
				return buf.Write(p)
			})
			str.EXPECT().Close()
			str.EXPECT().Read(gomock.Any()).Return(0, errors.New("test done"))
			_, err := client.RoundTrip(request)
			Expect(err).To(MatchError("test done"))
			hfs := decodeHeader(buf)
			Expect(hfs).To(HaveKeyWithValue(":scheme", "https"))
			Expect(hfs).To(HaveKeyWithValue(":method", "GET"))
			Expect(hfs).To(HaveKeyWithValue(":authority", "quic.clemente.io:1337"))
			Expect(hfs).To(HaveKeyWithValue(":path", "/file1.dat"))
		})

		It("returns a response", func() {
			rspBuf := &bytes.Buffer{}
			rw := newResponseWriter(rspBuf, utils.DefaultLogger)
			rw.WriteHeader(418)

			sess.EXPECT().OpenStreamSync().Return(str, nil)
			str.EXPECT().Write(gomock.Any()).AnyTimes()
			str.EXPECT().Close()
			str.EXPECT().Read(gomock.Any()).DoAndReturn(func(p []byte) (int, error) {
				return rspBuf.Read(p)
			}).AnyTimes()
			rsp, err := client.RoundTrip(request)
			Expect(err).ToNot(HaveOccurred())
			Expect(rsp.Proto).To(Equal("HTTP/3"))
			Expect(rsp.ProtoMajor).To(Equal(3))
			Expect(rsp.StatusCode).To(Equal(418))
		})

		Context("validating the address", func() {
			It("refuses to do requests for the wrong host", func() {
				req, err := http.NewRequest("https", "https://quic.clemente.io:1336/foobar.html", nil)
				Expect(err).ToNot(HaveOccurred())
				_, err = client.RoundTrip(req)
				Expect(err).To(MatchError("http3 client BUG: RoundTrip called for the wrong client (expected quic.clemente.io:1337, got quic.clemente.io:1336)"))
			})

			It("refuses to do plain HTTP requests", func() {
				req, err := http.NewRequest("https", "http://quic.clemente.io:1337/foobar.html", nil)
				Expect(err).ToNot(HaveOccurred())
				_, err = client.RoundTrip(req)
				Expect(err).To(MatchError("http3: unsupported scheme"))
			})
		})

		Context("requests containing a Body", func() {
			var strBuf *bytes.Buffer

			BeforeEach(func() {
				strBuf = &bytes.Buffer{}
				sess.EXPECT().OpenStreamSync().Return(str, nil)
				body := &mockBody{}
				body.SetData([]byte("request body"))
				var err error
				request, err = http.NewRequest("POST", "https://quic.clemente.io:1337/upload", body)
				Expect(err).ToNot(HaveOccurred())
				str.EXPECT().Write(gomock.Any()).DoAndReturn(func(p []byte) (int, error) {
					return strBuf.Write(p)
				}).AnyTimes()
			})

			It("sends a request", func() {
				done := make(chan struct{})
				str.EXPECT().Close().Do(func() { close(done) })
				// the response body is sent asynchronously, while already reading the response
				str.EXPECT().Read(gomock.Any()).DoAndReturn(func([]byte) (int, error) {
					<-done
					return 0, errors.New("test done")
				})
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError("test done"))
				hfs := decodeHeader(strBuf)
				Expect(hfs).To(HaveKeyWithValue(":method", "POST"))
				Expect(hfs).To(HaveKeyWithValue(":path", "/upload"))
			})

			It("returns the error that occurred when reading the body", func() {
				request.Body.(*mockBody).readErr = errors.New("testErr")
				done := make(chan struct{})
				str.EXPECT().CancelWrite(quic.ErrorCode(errorRequestCanceled)).Do(func(quic.ErrorCode) {
					close(done)
				})
				// the response body is sent asynchronously, while already reading the response
				str.EXPECT().Read(gomock.Any()).DoAndReturn(func([]byte) (int, error) {
					<-done
					return 0, errors.New("test done")
				})
				_, err := client.RoundTrip(request)
				Expect(err).To(MatchError("test done"))
			})
		})
	})
})
