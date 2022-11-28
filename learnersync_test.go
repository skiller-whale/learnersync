package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
)

func TestIgnorable(t *testing.T) {
	s := Sync{
		Ignore: []string{"*/.foo/*"},
	}
	for _, testStr := range []string{".foo", "foo", ".", "fine", "", ".foobar", "..foo", "///.foo///a"} {
		if s.ignorable(testStr) {
			t.Fatal("ignorable should be false for", testStr, "but wasn't")
		}
	}
	for _, testStr := range []string{"a/.foo/b"} {
		if !s.ignorable(testStr) {
			t.Fatal("ignorable should be true for", testStr, "but wasn't")
		}
	}
}

// utility for initialising a Sync alongside a test web server
// returns a listening web server, a channel on which all incoming requests can be read, and a matching Sync object which will connect to it.
type cachedHttpRequest struct {
	req  *http.Request
	body []byte
}

func mockServerAndSync() (*http.Server, chan cachedHttpRequest, *Sync) {
	l, err := net.Listen("tcp", ":8901")
	if err != nil {
		panic(err)
	}
	c := make(chan cachedHttpRequest, 1)
	s := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				panic(err)
			}
			c <- cachedHttpRequest{
				req:  req,
				body: body,
			}
			w.Write([]byte{})
		}),
	}
	go s.Serve(l)
	return &s, c, &Sync{ServerUrl: "http://localhost:8901"}
}

func TestHTTPFunctions(t *testing.T) {
	server, reqs, sync := mockServerAndSync()
	defer server.Close()
	sync.AttendanceId = "testid"

	if err := sync.PostPing(); err != nil {
		t.Fatalf("ping didn't work: %v", err)
	}

	if r, ok := <-reqs; !ok || r.req.URL.Path != "/attendances/testid/pings" {
		t.Fatal("ping path for testid was incorrect", r.req.URL.Path)
	}

	fileContents := "Hello\nThere\t\t😀"

	f, err := os.CreateTemp("", "topost")
	defer os.Remove(f.Name())
	f.Write(([]byte)(fileContents))
	f.Close()
	if err != nil {
		panic(err)
	}
	if err := sync.PostFile(f.Name()); err != nil {
		t.Fatalf("posting file didn't work: %v", err)
	}

	if r, ok := <-reqs; !ok || r.req.URL.Path != "/attendances/testid/file_snapshots" {
		t.Fatal("snapshots path for testid was incorrect", r.req.URL.Path)
	} else {
		if ct := r.req.Header.Get("content-type"); ct != "application/json" {
			t.Fatal("posting file sent wrong content-type", ct)
		}

		decoder := json.NewDecoder(bytes.NewReader(r.body))
		j := struct {
			RelativePath string `json:"relative_path"`
			Contents     string `json:"contents"`
		}{}
		err := decoder.Decode(&j)
		if err != nil {
			t.Fatal("couldn't decode json request", err)
		}
		if j.RelativePath != f.Name() {
			t.Fatal("wrong filename on posted file", j.RelativePath, "not", f.Name())
		}
		if j.Contents != fileContents {
			t.Fatal("wrong contents on posted file", j.Contents, "not", fileContents)
		}
	}

	// 0-length file should not cause a Post, how to test
}
