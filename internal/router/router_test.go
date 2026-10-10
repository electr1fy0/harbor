package router

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHostRouterRoutesByHostAndPort(t *testing.T) {
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("server A"))
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("server B"))
	}))
	defer serverB.Close()

	r := New()
	if err := r.SetRoute("a.localhost", serverA.URL); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRoute("b.localhost", serverB.URL); err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(r)
	defer proxy.Close()

	for _, test := range []struct {
		name string
		host string
		want string
	}{
		{name: "a", host: "A.LOCALHOST", want: "server A"},
		{name: "b with port", host: "b.localhost:8080", want: "server B"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, proxy.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Host = test.host
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(body); got != test.want {
				t.Fatalf("body = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSwitchRouteKeepsServingDuringCommit(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		t.Run(fmt.Sprintf("commit fails=%v", failCommit), func(t *testing.T) {
			old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "old")
			}))
			defer old.Close()
			candidate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "candidate")
			}))
			defer candidate.Close()
			r := New()
			for _, host := range []string{"app.localhost", "other.localhost"} {
				if err := r.SetRoute(host, old.URL); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			commitErr := errors.New("commit failed")
			result := make(chan error, 1)
			go func() {
				_, err := r.SwitchRoute("app.localhost", candidate.URL, func() error {
					close(entered)
					<-release
					if failCommit {
						return commitErr
					}
					return nil
				})
				result <- err
			}()
			<-entered
			responses := make(chan *httptest.ResponseRecorder, 2)
			for _, host := range []string{"app.localhost", "other.localhost"} {
				go func(host string) {
					request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
					response := httptest.NewRecorder()
					r.ServeHTTP(response, request)
					responses <- response
				}(host)
			}
			for i := 0; i < 2; i++ {
				select {
				case response := <-responses:
					if response.Code != http.StatusOK || response.Body.String() != "old" {
						t.Errorf("request during commit: %d %q", response.Code, response.Body.String())
					}
				case <-time.After(time.Second):
					t.Error("durable commit blocked proxy traffic")
				}
			}
			close(release)
			if err := <-result; failCommit && !errors.Is(err, commitErr) || !failCommit && err != nil {
				t.Fatalf("SwitchRoute error = %v", err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://app.localhost/", nil)
			response := httptest.NewRecorder()
			r.ServeHTTP(response, request)
			want := "candidate"
			if failCommit {
				want = "old"
			}
			if response.Body.String() != want {
				t.Fatalf("route after commit = %q, want %q", response.Body.String(), want)
			}
		})
	}
}

func TestHostRouterUnknownAndReplacement(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("first"))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("second"))
	}))
	defer second.Close()

	r := New()
	proxy := httptest.NewServer(r)
	defer proxy.Close()

	response, err := http.Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNotFound {
		response.Body.Close()
		t.Fatalf("unknown host status = %d, want %d", response.StatusCode, http.StatusNotFound)
	}
	response.Body.Close()

	if err := r.SetRoute("app.localhost", first.URL); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRoute("app.localhost", second.URL); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodGet, proxy.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = "app.localhost"
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "second" {
		t.Fatalf("replacement body = %q, want second", got)
	}
}

func TestSetRouteRejectsInvalidUpstream(t *testing.T) {
	r := New()
	for _, upstream := range []string{"", "ftp://example.com", "http://"} {
		t.Run(fmt.Sprintf("%q", upstream), func(t *testing.T) {
			if err := r.SetRoute("app.localhost", upstream); err == nil {
				t.Fatalf("SetRoute(%q) unexpectedly succeeded", upstream)
			}
		})
	}
}
