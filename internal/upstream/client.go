package upstream

import (
	"bufio"
	"context"
	standardtls "crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// NewHTTPClient returns the shared outbound client policy used for OpenAI
// authentication, quota, model, and inference requests. When chromeTLS is
// enabled, only the official ChatGPT HTTPS host uses a Chrome-style TLS
// ClientHello; every other host keeps Go's standard transport behavior.
func NewHTTPClient(timeout time.Duration, chromeTLS bool) *http.Client {
	transport := http.RoundTripper(http.DefaultTransport)
	if chromeTLS {
		transport = &selectiveRoundTripper{
			chrome:   &chromeRoundTripper{proxy: http.ProxyFromEnvironment},
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
type chromeRoundTripper struct {
	proxy func(*http.Request) (*url.URL, error)
}

func (t *chromeRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	hostname := request.URL.Hostname()
	address := request.URL.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(hostname, "443")
	}

	rawConnection, err := t.dialConnection(request, address)
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

func (t *chromeRoundTripper) dialConnection(request *http.Request, address string) (net.Conn, error) {
	direct := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	proxyResolver := t.proxy
	if proxyResolver == nil {
		proxyResolver = http.ProxyFromEnvironment
	}
	proxyURL, err := proxyResolver(request)
	if err != nil {
		return nil, fmt.Errorf("resolve ChatGPT upstream proxy: %w", err)
	}
	if proxyURL == nil {
		return direct.DialContext(request.Context(), "tcp", address)
	}

	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https":
		return dialHTTPConnectProxy(request.Context(), direct, proxyURL, address)
	case "socks5", "socks5h":
		proxyDialer, proxyErr := proxy.FromURL(proxyURL, direct)
		if proxyErr != nil {
			return nil, fmt.Errorf("configure ChatGPT SOCKS proxy: %w", proxyErr)
		}
		contextDialer, ok := proxyDialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("ChatGPT SOCKS proxy does not support context cancellation")
		}
		return contextDialer.DialContext(request.Context(), "tcp", address)
	default:
		return nil, fmt.Errorf("unsupported ChatGPT upstream proxy scheme %q", proxyURL.Scheme)
	}
}

func dialHTTPConnectProxy(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, address string) (net.Conn, error) {
	proxyAddress := proxyURL.Host
	if _, _, err := net.SplitHostPort(proxyAddress); err != nil {
		port := "80"
		if strings.EqualFold(proxyURL.Scheme, "https") {
			port = "443"
		}
		proxyAddress = net.JoinHostPort(proxyURL.Hostname(), port)
	}
	connection, err := dialer.DialContext(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("dial ChatGPT upstream proxy: %w", err)
	}
	closeConnection := true
	defer func() {
		if closeConnection {
			_ = connection.Close()
		}
	}()
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = connection.Close()
	})
	defer stopCancellation()
	if strings.EqualFold(proxyURL.Scheme, "https") {
		tlsConnection := standardtls.Client(connection, &standardtls.Config{ServerName: proxyURL.Hostname(), MinVersion: standardtls.VersionTLS12})
		if err = tlsConnection.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("handshake with ChatGPT upstream proxy: %w", err)
		}
		connection = tlsConnection
	}

	connectRequest := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}
	connectRequest.Header.Set("User-Agent", "CodexOne")
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credentials := proxyURL.User.Username() + ":" + password
		connectRequest.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	deadline := time.Now().Add(30 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set ChatGPT upstream proxy deadline: %w", err)
	}
	if err = connectRequest.Write(connection); err != nil {
		return nil, fmt.Errorf("send ChatGPT upstream proxy CONNECT: %w", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, connectRequest)
	if err != nil {
		return nil, fmt.Errorf("read ChatGPT upstream proxy CONNECT: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("ChatGPT upstream proxy CONNECT returned %s", response.Status)
	}
	if err = connection.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear ChatGPT upstream proxy deadline: %w", err)
	}
	closeConnection = false
	if reader.Buffered() > 0 {
		return &bufferedConnection{Conn: connection, reader: reader}, nil
	}
	return connection, nil
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
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
