package upstream

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestChromeRoundTripperHonorsHTTPProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverErr := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer connection.Close()
		request, readErr := http.ReadRequest(bufio.NewReader(connection))
		if readErr != nil {
			serverErr <- readErr
			return
		}
		if request.Method != http.MethodConnect || request.Host != "chatgpt.com:443" {
			serverErr <- fmt.Errorf("CONNECT request = %s %s", request.Method, request.Host)
			return
		}
		wantAuthorization := "Basic " + base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-pass"))
		if request.Header.Get("Proxy-Authorization") != wantAuthorization {
			serverErr <- fmt.Errorf("Proxy-Authorization = %q", request.Header.Get("Proxy-Authorization"))
			return
		}
		if _, writeErr := io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n"); writeErr != nil {
			serverErr <- writeErr
			return
		}
		buffer := make([]byte, 4)
		if _, readErr = io.ReadFull(connection, buffer); readErr != nil {
			serverErr <- readErr
			return
		}
		if string(buffer) != "ping" {
			serverErr <- fmt.Errorf("tunnel payload = %q", buffer)
			return
		}
		serverErr <- nil
	}()

	proxyURL, err := url.Parse("http://proxy-user:proxy-pass@" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport := &chromeRoundTripper{proxy: func(*http.Request) (*url.URL, error) { return proxyURL, nil }}
	request, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := transport.dialConnection(request, "chatgpt.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(connection, "ping"); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	_ = connection.Close()
	select {
	case err = <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not receive CONNECT tunnel traffic")
	}
}

func TestSelectiveRoundTripperUsesChromeOnlyForExactChatGPTHost(t *testing.T) {
	var selected string
	response := func() *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}
	}
	transport := &selectiveRoundTripper{
		chrome: roundTripFunc(func(*http.Request) (*http.Response, error) {
			selected = "chrome"
			return response(), nil
		}),
		fallback: roundTripFunc(func(*http.Request) (*http.Response, error) {
			selected = "fallback"
			return response(), nil
		}),
	}

	tests := []struct {
		url  string
		want string
	}{
		{url: "https://chatgpt.com/backend-api/codex/responses", want: "chrome"},
		{url: "http://chatgpt.com/backend-api/codex/responses", want: "fallback"},
		{url: "https://chatgpt.com.example/backend-api/codex/responses", want: "fallback"},
		{url: "https://example.com/backend-api/codex/responses", want: "fallback"},
	}
	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			selected = ""
			request, err := http.NewRequest(http.MethodGet, test.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = result.Body.Close()
			if selected != test.want {
				t.Fatalf("selected transport = %q, want %q", selected, test.want)
			}
		})
	}
}
