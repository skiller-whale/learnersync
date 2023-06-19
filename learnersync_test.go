package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
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

type rig struct {
	server *http.Server
	sync   *Sync
	c      chan cachedHttpRequest
}

func (r rig) nextRequest() cachedHttpRequest {
	select {
	case req := <-r.c:
		return req
	case <-time.After(500 * time.Millisecond):
		panic("no request received")
	}
}

func initRig(t *testing.T) (r rig) {
	t.Helper()
	t.Setenv("WATCHER_BASE_PATH", t.TempDir())

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
			w.Write([]byte("hello"))
		}),
	}
	go s.Serve(l)
	if testing.Verbose() {
		t.Setenv("DEBUG", "nopings fsevents")
	} else {
		t.Setenv("DEBUG", "nopings quiet")
	}
	t.Setenv("WATCHED_EXTS", "txt")
	t.Setenv("SERVER_URL", "http://localhost:8901")
	sync, err := InitFromEnv()
	if err != nil {
		panic(err)
	}
	go sync.Run()

	t.Cleanup(func() {
		sync.Close()
		s.Close()
	})
	r = rig{&s, &sync, c}
	_ = r.nextRequest() // Ignore first request to check server is valid
	return rig{&s, &sync, c}
}

type fileUploadBody struct {
	RelativePath string `json:"relative_path"`
	Contents     string `json:"contents"`
}

func (fub *fileUploadBody) parse(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	return decoder.Decode(&fub)
}

func TestHTTPFunctions(t *testing.T) {
	rig := initRig(t)

	rig.sync.AttendanceId = "testid"

	if err := rig.sync.PostPing(); err != nil {
		t.Fatalf("ping didn't work: %v", err)
	}

	if chr := rig.nextRequest(); chr.req.URL.Path != "/attendances/testid/pings" {
		t.Fatal("ping path for testid was incorrect", chr.req.URL.Path)
	}

	fileContents := "Hello\nThere\t\t😀"

	f, err := os.CreateTemp("", "topost")
	defer os.Remove(f.Name())
	f.Write(([]byte)(fileContents))
	f.Close()
	if err != nil {
		panic(err)
	}
	if err := rig.sync.PostFile(f.Name()); err != nil {
		t.Fatalf("posting file didn't work: %v", err)
	}

	if chr := rig.nextRequest(); chr.req.URL.Path != "/attendances/testid/file_snapshots" {
		t.Fatal("snapshots path for testid was incorrect", chr.req.URL.Path)
	} else {
		if ct := chr.req.Header.Get("content-type"); ct != "application/json" {
			t.Fatal("posting file sent wrong content-type", ct)
		}

		var fub fileUploadBody
		if err := fub.parse(chr.body); err != nil {
			t.Fatal("couldn't decode json request", err)
		}

		if fub.RelativePath != f.Name() {
			t.Fatal("wrong filename on posted file", fub.RelativePath, "not", f.Name())
		}
		if fub.Contents != fileContents {
			t.Fatal("wrong contents on posted file", fub.Contents, "not", fileContents)
		}
	}

	// 0-length file should not cause a Post, how to test
}

func TestMkdirAll(t *testing.T) {
	rig := initRig(t)

	subPath := os.Getenv("WATCHER_BASE_PATH") + "/1/2/3/4"
	os.MkdirAll(subPath, 0777)
	foo, err := os.Create(subPath + "/test.txt")
	if err != nil {
		panic(err)
	}
	foo.Write([]byte("Testing file write"))
	foo.Close()

	chr := rig.nextRequest()
	var fub fileUploadBody
	if err := fub.parse(chr.body); err != nil {
		t.Fatal("couldn't decode json request", err)
	}
}
