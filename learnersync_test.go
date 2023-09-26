package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

type MatchesExtTest = struct {
	watchedExts []string
	path        string
	expected    bool
}

var MatchesExtTests = []MatchesExtTest{
	{[]string{}, "file.js", false},                          // no watched extensions
	{[]string{""}, "file.js", false},                        // empty watched extension
	{[]string{"js"}, "file.js", true},                       // single watched extension
	{[]string{"js"}, "", false},                             // empty path
	{[]string{"js"}, "/a/long/path/file.js", true},          // single watched extension with longer path
	{[]string{"js"}, ".js", true},                           // no filename before .
	{[]string{"longext", "js"}, "file.js", true},            // multiple watched extensions
	{[]string{"longext", "js"}, "longext.file", false},      // string match elsewhere in path
	{[]string{"longext", "js"}, "file.long", false},         // partial extension match
	{[]string{"Dockerfile"}, "Dockerfile", true},            // file with no extension
	{[]string{"Dockerfile"}, "/Dockerfile", true},           // file with no extension
	{[]string{"Dockerfile"}, "/some/path/Dockerfile", true}, // file with no extension
	{[]string{".js"}, "file.js", true},                      // with period included
}

func TestMatchesExts(t *testing.T) {
	for _, test := range MatchesExtTests {
		s := Sync{
			WatchedExts: test.watchedExts,
		}
		if s.MatchesExts(test.path) != test.expected {
			t.Fatal("matchesExt failed for", test.path, "with watchedExts", test.watchedExts)
		}
	}
}

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
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	port := l.Addr().(*net.TCPAddr).Port

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

	return &s, c, &Sync{ServerUrl: fmt.Sprintf("http://localhost:%v", port)}
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

type HLEParameterTest = struct {
	setEnv   func(s *Sync)
	expected bool
}

var HLEParameterTests = []HLEParameterTest{
	{func(s *Sync) { s.InHostedEnv = false }, false},
	{func(s *Sync) { s.InHostedEnv = true }, true},
}

func TestPostPingUsesHostedEnvironmentEnvvar(t *testing.T) {
	server, reqs, sync := mockServerAndSync()
	defer server.Close()

	for _, test := range HLEParameterTests {
		test.setEnv(sync) // set up the hosted learner environment env var

		if err := sync.PostPing(); err != nil {
			t.Fatalf("ping didn't work: %v", err)
		}

		if r, ok := <-reqs; !ok {
			t.Fatal("no request sent by PostPing")
		} else {
			decoder := json.NewDecoder(bytes.NewReader(r.body))
			j := struct {
				SentFromHostedEnvironment bool `json:"sent_from_hosted_environment"`
			}{}
			if err := decoder.Decode(&j); err != nil {
				t.Fatal("couldn't decode json", err)
			}
			if j.SentFromHostedEnvironment != test.expected {
				t.Fatal("wrong value for sent from hosted environment", j.SentFromHostedEnvironment, "not", test.expected)
			}
		}
	}
}

func TestPostFileUsesHostedEnvironmentEnvvar(t *testing.T) {
	server, reqs, sync := mockServerAndSync()
	defer server.Close()

	for _, test := range HLEParameterTests {
		test.setEnv(sync) // set up the hosted learner environment env var

		var filename string
		if f, err := os.CreateTemp("", "topost"); err != nil {
			panic(err)
		} else {
			defer os.Remove(f.Name())
			f.Write(([]byte)("Hello\nThere\t\t😀"))
			f.Close()
			filename = f.Name()
		}

		if err := sync.PostFile(filename); err != nil {
			t.Fatalf("Posting file didn't work: %v", err)
		}

		if r, ok := <-reqs; !ok {
			t.Fatal("no request sent by PostFile")
		} else {

			decoder := json.NewDecoder(bytes.NewReader(r.body))
			j := struct {
				SentFromHostedEnvironment bool `json:"sent_from_hosted_environment"`
			}{}
			if err := decoder.Decode(&j); err != nil {
				t.Fatal("couldn't decode json request", err)
			}
			if j.SentFromHostedEnvironment != test.expected {
				t.Fatal("wrong value for sent from hosted environment", j.SentFromHostedEnvironment, "not", test.expected)
			}
		}
	}
}

func TestFSEvents(t *testing.T) {
	dir := fmt.Sprintf("%s/testFsEvents.%d.%d", os.TempDir(), os.Getpid(), rand.Int())
	fatalIfSet(os.Mkdir(dir, 0755))
	defer os.RemoveAll(dir)
	watcher, err := fsnotify.NewWatcher()
	fatalIfSet(err)
	watcher.Add(dir)
	f, err := os.CreateTemp(dir, "test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-watcher.Events:
		if event.Name == f.Name() {
			return
		}
	case <-time.NewTicker(time.Second).C:
		t.Fatalf("fsevents doesn't seem to work on this platform")
	}
}
