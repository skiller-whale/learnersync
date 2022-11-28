package main

import (
	"net"
	"net/http"
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

func mockServer() (*http.Server, chan *http.Request) {
	l, err := net.Listen("tcp", ":8901")
	if err != nil {
		panic(err)
	}
	c := make(chan *http.Request, 1)
	s := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c <- req
			w.Write([]byte{})
		}),
	}
	go s.Serve(l)
	return &s, c
}

func TestHTTPFunctions(t *testing.T) {
	server, reqs := mockServer()
	defer server.Close()

	sync := Sync{
		ServerUrl:    "http://localhost:8901",
		AttendanceId: "testid",
	}

	if err := sync.PostPing(); err != nil {
		t.Fatalf("ping didn't work: %v", err)
	}
	req := <-reqs
	if req.URL.Path != "/attendances/testid/pings" {
		t.Fatal("ping path for testid was incorrect", req.URL.Path)
	}
}
