package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientManualRedirectAndSetCookie(t *testing.T) {
	var seenCookie string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			w.Header().Add("Set-Cookie", "sid=one; Path=/")
			w.Header().Set("Location", "/next")
			w.WriteHeader(302)
		case "/next":
			seenCookie = r.Header.Get("Cookie")
			w.Header().Set("Location", "/final")
			w.WriteHeader(303)
		case "/final":
			fmt.Fprint(w, "ok")
		}
	}))
	defer s.Close()
	c := NewClient(nil)
	got, e := c.Follow(context.Background(), Request{Method: "POST", URL: s.URL + "/start", Body: []byte("x")}, 5)
	if e != nil {
		t.Fatal(e)
	}
	if got.FinalURL != s.URL+"/final" || got.Response.Text() != "ok" {
		t.Fatalf("result=%#v", got)
	}
	if seenCookie != "sid=one" {
		t.Fatalf("cookie=%q", seenCookie)
	}
	if len(got.History) != 3 {
		t.Fatalf("history=%d", len(got.History))
	}
}

func TestClientRedirect307PreservesBody(t *testing.T) {
	var method, body string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			w.Header().Set("Location", "/b")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		method = r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte("done"))
	}))
	defer s.Close()
	c := NewClient(nil)
	got, err := c.Follow(context.Background(), Request{Method: http.MethodPost, URL: s.URL + "/a", Body: []byte("payload")}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Response.Text() != "done" || method != http.MethodPost || body != "payload" {
		t.Fatalf("method=%s body=%q result=%q", method, body, got.Response.Text())
	}
}
