package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
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
