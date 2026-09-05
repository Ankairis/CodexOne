package upstream

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// NewHTTPClient returns the shared outbound client policy used for OpenAI
// authentication, quota, model, and inference requests. When chromeTLS is
// enabled, only the official ChatGPT HTTPS host uses a Chrome-style TLS
// ClientHello; every other host keeps Go's standard transport behavior.
func NewHTTPClient(timeout time.Duration, chromeTLS bool) *http.Client {
	transport := http.RoundTripper(http.DefaultTransport)
	if chromeTLS {
		transport = &selectiveRoundTripper{
			chrome:   &chromeRoundTripper{},
			fallback: http.DefaultTransport,
		}
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

type selectiveRoundTripper struct {
	chrome   http.RoundTripper
	fallback http.RoundTripper
}

func (t *selectiveRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil {
		return nil, fmt.Errorf("upstream transport: request is nil")
	}
	if isChatGPTHTTPS(request) {
		return t.chrome.RoundTrip(request)
	}
	return t.fallback.RoundTrip(request)
}

func isChatGPTHTTPS(request *http.Request) bool {
	return request != nil && request.URL != nil &&
		strings.EqualFold(request.URL.Scheme, "https") &&
		strings.EqualFold(request.URL.Hostname(), "chatgpt.com")
}

// chromeRoundTripper performs one request per Chrome-profiled TLS connection.
// A dedicated connection keeps cancellation and streaming ownership simple and
// avoids mixing the special transport with requests for any other host.
type chromeRoundTripper struct{}

func (t *chromeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	hostname := request.URL.Hostname()
	address := request.URL.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(hostname, "443")
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	rawConnection, err := dialer.DialContext(request.Context(), "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial ChatGPT upstream: %w", err)
	}
	closeRaw := true
	defer func() {
		if closeRaw {
			_ = rawConnection.Close()
		}
	}()

	tlsConnection := tls.UClient(rawConnection, &tls.Config{ServerName: hostname}, tls.HelloChrome_Auto)
	if err = tlsConnection.HandshakeContext(request.Context()); err != nil {
		return nil, fmt.Errorf("handshake with ChatGPT upstream: %w", err)
	}
	if protocol := tlsConnection.ConnectionState().NegotiatedProtocol; protocol != "h2" {
		return nil, fmt.Errorf("ChatGPT upstream negotiated unsupported protocol %q", protocol)
	}

	transport := &http2.Transport{}
	connection, err := transport.NewClientConn(tlsConnection)
	if err != nil {
		return nil, fmt.Errorf("initialize ChatGPT HTTP/2 connection: %w", err)
	}
	response, err := connection.RoundTrip(request)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("send ChatGPT upstream request: %w", err)
	}
	if response == nil || response.Body == nil {
		_ = connection.Close()
		return nil, fmt.Errorf("ChatGPT upstream returned an empty response")
	}

	closeRaw = false
	response.Body = &connectionBody{
		ReadCloser: response.Body,
		close: func() error {
			return connection.Close()
		},
	}
	return response, nil
}

type connectionBody struct {
	io.ReadCloser
	close func() error
	once  sync.Once
	err   error
}

func (b *connectionBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		var bodyErr error
		if b.ReadCloser != nil {
			bodyErr = b.ReadCloser.Close()
		}
		var connectionErr error
		if b.close != nil {
			connectionErr = b.close()
		}
		b.err = errors.Join(bodyErr, connectionErr)
	})
	return b.err
}
