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
	"path/filepath"
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

func TestIgnorableWithTrailingSlash(t *testing.T) {
	testCases := []struct {
		pattern       string
		shouldIgnore  []string
		shouldNotIgnore []string
	}{
		{
			pattern: "node_modules/",
			shouldIgnore: []string{
				"/app/exercises/node_modules",
				"/app/exercises/src/node_modules",
				"/app/exercises/node_modules/package",
				"/app/exercises/node_modules/package/file.js",
			},
			shouldNotIgnore: []string{
				"/app/exercises/src",
				"/app/exercises/my_node_modules",
				"/app/exercises/not_node_modules",
				"/app/exercises/node_modules_backup",
			},
		},
		{
			pattern: "bin/",
			shouldIgnore: []string{
				"/app/exercises/bin",
				"/app/exercises/project/bin",
				"/app/exercises/bin/output.dll",
			},
			shouldNotIgnore: []string{
				"/app/exercises/binary",
				"/app/exercises/combined",
				"/app/exercises/robin",
				"/app/exercises/robin/file.js",
			},
		},
		{
			pattern: "obj/",
			shouldIgnore: []string{
				"/app/exercises/obj",
				"/app/exercises/project/obj",
				"/app/exercises/obj/Debug",
			},
			shouldNotIgnore: []string{
				"/app/exercises/object",
				"/app/exercises/src",
				"/app/exercises/objection",
			},
		},
	}

	for _, tc := range testCases {
		s := Sync{
			Ignore: []string{tc.pattern},
		}

		for _, path := range tc.shouldIgnore {
			if !s.ignorable(path) {
				t.Errorf("Pattern %q should ignore %q but didn't", tc.pattern, path)
			}
		}

		for _, path := range tc.shouldNotIgnore {
			if s.ignorable(path) {
				t.Errorf("Pattern %q should NOT ignore %q but did", tc.pattern, path)
			}
		}
	}
}

func TestIgnorableWithLeadingSlash(t *testing.T) {
	testCases := []struct {
		pattern         string
		shouldIgnore    []string
		shouldNotIgnore []string
	}{
		{
			pattern: "/lib",
			shouldIgnore: []string{
				"/lib",
				"/lib/file.js",
				"/lib/nested/file.js",
			},
			shouldNotIgnore: []string{
				"/app/lib",
				"/app/lib/file.js",
				"/app/exercises/lib",
				"/app/exercises/lib/file.js",
			},
		},
		{
			pattern: "/lib/",
			shouldIgnore: []string{
				"/lib",
				"/lib/file.js",
				"/lib/nested/file.js",
			},
			shouldNotIgnore: []string{
				"/app/lib",
				"/app/lib/file.js",
				"/app/exercises/lib",
				"/app/exercises/lib/file.js",
			},
		},
		{
			pattern: "/node_modules",
			shouldIgnore: []string{
				"/node_modules",
				"/node_modules/package",
				"/node_modules/package/file.js",
			},
			shouldNotIgnore: []string{
				"/app/node_modules",
				"/app/node_modules/file.js",
				"/app/exercises/node_modules",
			},
		},
	}

	for _, tc := range testCases {
		s := Sync{
			Ignore: []string{tc.pattern},
		}

		for _, path := range tc.shouldIgnore {
			if !s.ignorable(path) {
				t.Errorf("Pattern %q should ignore %q but didn't", tc.pattern, path)
			}
		}

		for _, path := range tc.shouldNotIgnore {
			if s.ignorable(path) {
				t.Errorf("Pattern %q should NOT ignore %q but did", tc.pattern, path)
			}
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
	// Cycle through available ports to ensure no clashes or delays between tests.
	// We could alternatively try keeping the port the same but setting
	// SO_REUSEADDR on the TCP listener.
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

// Test readFileMax function
func TestReadFileMax(t *testing.T) {
	t.Run("reads file smaller than max", func(t *testing.T) {
		f, err := os.CreateTemp("", "small")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())

		content := "Hello, World!"
		f.Write([]byte(content))
		f.Close()

		data, err := readFileMax(f.Name(), 100)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if string(data) != content {
			t.Fatalf("expected %q, got %q", content, string(data))
		}
	})

	t.Run("returns TOO_LARGE_ERROR when file exceeds max", func(t *testing.T) {
		f, err := os.CreateTemp("", "large")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())

		content := "Hello, World!"
		f.Write([]byte(content))
		f.Close()

		_, err = readFileMax(f.Name(), 5)
		if err != TOO_LARGE_ERROR {
			t.Fatalf("expected TOO_LARGE_ERROR, got %v", err)
		}
	})

	t.Run("handles empty file", func(t *testing.T) {
		f, err := os.CreateTemp("", "empty")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		f.Close()

		data, err := readFileMax(f.Name(), 100)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(data) != 0 {
			t.Fatalf("expected empty data, got %d bytes", len(data))
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		_, err := readFileMax("/non/existent/file", 100)
		if err == nil {
			t.Fatal("expected error for non-existent file")
		}
	})
}

// Test decodeBytes function
func TestDecodeBytes(t *testing.T) {
	t.Run("removes UTF-8 BOM", func(t *testing.T) {
		input := append([]byte{0xef, 0xbb, 0xbf}, []byte("Hello")...)
		result := decodeBytes(input)
		if result != "Hello" {
			t.Fatalf("expected %q, got %q", "Hello", result)
		}
	})

	t.Run("handles text without BOM", func(t *testing.T) {
		input := []byte("Hello, World!")
		result := decodeBytes(input)
		if result != "Hello, World!" {
			t.Fatalf("expected %q, got %q", "Hello, World!", result)
		}
	})

	t.Run("handles short input", func(t *testing.T) {
		input := []byte("Hi")
		result := decodeBytes(input)
		if result != "Hi" {
			t.Fatalf("expected %q, got %q", "Hi", result)
		}
	})

	t.Run("handles empty input", func(t *testing.T) {
		input := []byte{}
		result := decodeBytes(input)
		if result != "" {
			t.Fatalf("expected empty string, got %q", result)
		}
	})
}

// Test postJSON error handling
func TestPostJSONErrorHandling(t *testing.T) {
	t.Run("returns nil for 200 OK", func(t *testing.T) {
		l, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port

		server := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		}
		go server.Serve(l)
		defer server.Close()

		sync := &Sync{
			ServerUrl:    fmt.Sprintf("http://localhost:%d", port),
			AttendanceId: "testid",
		}

		err = sync.postJSON("test", "{}")
		if err != nil {
			t.Fatalf("expected no error for 200 OK, got %v", err)
		}
	})

	t.Run("returns CLIENT_ERROR for 400", func(t *testing.T) {
		l, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port

		server := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("Bad request"))
			}),
		}
		go server.Serve(l)
		defer server.Close()

		sync := &Sync{
			ServerUrl:    fmt.Sprintf("http://localhost:%d", port),
			AttendanceId: "testid",
		}

		err = sync.postJSON("test", "{}")
		if err != CLIENT_ERROR {
			t.Fatalf("expected CLIENT_ERROR for 400, got %v", err)
		}
	})

	t.Run("returns THROTTLED_ERROR for 429", func(t *testing.T) {
		l, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port

		server := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte("Too many requests"))
			}),
		}
		go server.Serve(l)
		defer server.Close()

		sync := &Sync{
			ServerUrl:    fmt.Sprintf("http://localhost:%d", port),
			AttendanceId: "testid",
		}

		err = sync.postJSON("test", "{}")
		if err != THROTTLED_ERROR {
			t.Fatalf("expected THROTTLED_ERROR for 429, got %v", err)
		}
	})

	t.Run("returns SERVER_ERROR for 500", func(t *testing.T) {
		l, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port

		server := &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}),
		}
		go server.Serve(l)
		defer server.Close()

		sync := &Sync{
			ServerUrl:    fmt.Sprintf("http://localhost:%d", port),
			AttendanceId: "testid",
		}

		err = sync.postJSON("test", "{}")
		if err != SERVER_ERROR {
			t.Fatalf("expected SERVER_ERROR for 500, got %v", err)
		}
	})
}

// Test WatchedFiles.AddedOrChanged
func TestWatchedFilesAddedOrChanged(t *testing.T) {
	baseTime := time.Now()

	t.Run("detects new files", func(t *testing.T) {
		prev := WatchedFiles{
			"/path/file1.txt": baseTime,
		}
		current := WatchedFiles{
			"/path/file1.txt": baseTime,
			"/path/file2.txt": baseTime.Add(time.Second),
		}

		changed := current.AddedOrChanged(prev)
		if len(changed) != 1 {
			t.Fatalf("expected 1 changed file, got %d", len(changed))
		}
		if changed[0] != "/path/file2.txt" {
			t.Fatalf("expected /path/file2.txt, got %s", changed[0])
		}
	})

	t.Run("detects modified files", func(t *testing.T) {
		prev := WatchedFiles{
			"/path/file1.txt": baseTime,
		}
		current := WatchedFiles{
			"/path/file1.txt": baseTime.Add(time.Second),
		}

		changed := current.AddedOrChanged(prev)
		if len(changed) != 1 {
			t.Fatalf("expected 1 changed file, got %d", len(changed))
		}
		if changed[0] != "/path/file1.txt" {
			t.Fatalf("expected /path/file1.txt, got %s", changed[0])
		}
	})

	t.Run("ignores unchanged files", func(t *testing.T) {
		prev := WatchedFiles{
			"/path/file1.txt": baseTime,
			"/path/file2.txt": baseTime,
		}
		current := WatchedFiles{
			"/path/file1.txt": baseTime,
			"/path/file2.txt": baseTime,
		}

		changed := current.AddedOrChanged(prev)
		if len(changed) != 0 {
			t.Fatalf("expected 0 changed files, got %d", len(changed))
		}
	})

	t.Run("handles empty previous state", func(t *testing.T) {
		prev := WatchedFiles{}
		current := WatchedFiles{
			"/path/file1.txt": baseTime,
			"/path/file2.txt": baseTime,
		}

		changed := current.AddedOrChanged(prev)
		if len(changed) != 2 {
			t.Fatalf("expected 2 changed files, got %d", len(changed))
		}
	})
}

// Test readAttendanceIdFile
func TestReadAttendanceIdFile(t *testing.T) {
	t.Run("trims whitespace from attendance ID", func(t *testing.T) {
		f, err := os.CreateTemp("", "attendance")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())

		f.Write([]byte("  test-id-123  \n\t"))
		f.Close()

		sync := &Sync{AttendanceIdFile: f.Name()}
		id, err := sync.readAttendanceIdFile()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "test-id-123" {
			t.Fatalf("expected %q, got %q", "test-id-123", id)
		}
	})

	t.Run("handles file with newlines", func(t *testing.T) {
		f, err := os.CreateTemp("", "attendance")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())

		f.Write([]byte("test-id\n\n\n"))
		f.Close()

		sync := &Sync{AttendanceIdFile: f.Name()}
		id, err := sync.readAttendanceIdFile()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "test-id" {
			t.Fatalf("expected %q, got %q", "test-id", id)
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		sync := &Sync{AttendanceIdFile: "/non/existent/file"}
		_, err := sync.readAttendanceIdFile()
		if err == nil {
			t.Fatal("expected error for non-existent file")
		}
	})
}

// Test PostFile with empty file
func TestPostFileWithEmptyFile(t *testing.T) {
	server, reqs, sync := mockServerAndSync()
	defer server.Close()
	sync.AttendanceId = "testid"

	f, err := os.CreateTemp("", "empty")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Close()

	err = sync.PostFile(f.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not post empty file - check that no request was sent
	select {
	case <-reqs:
		t.Fatal("empty file should not be posted")
	case <-time.After(100 * time.Millisecond):
		// No request received, as expected
	}
}

// Test PostFile with too-large file
func TestPostFileWithTooLargeFile(t *testing.T) {
	server, reqs, sync := mockServerAndSync()
	defer server.Close()
	sync.AttendanceId = "testid"

	f, err := os.CreateTemp("", "large")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	// Write more than MAX_UPLOAD_BYTES
	largeContent := make([]byte, MAX_UPLOAD_BYTES+1)
	for i := range largeContent {
		largeContent[i] = 'x'
	}
	f.Write(largeContent)
	f.Close()

	err = sync.PostFile(f.Name())
	if err != TOO_LARGE_ERROR {
		t.Fatalf("expected TOO_LARGE_ERROR, got %v", err)
	}

	// Should not post the file
	select {
	case <-reqs:
		t.Fatal("too-large file should not be posted")
	case <-time.After(100 * time.Millisecond):
		// No request received, as expected
	}
}

// Test ScanForFiles
func TestScanForFiles(t *testing.T) {
	dir := fmt.Sprintf("%s/testScanFiles.%d.%d", os.TempDir(), os.Getpid(), rand.Int())
	fatalIfSet(os.Mkdir(dir, 0755))
	defer os.RemoveAll(dir)

	// Create some test files
	os.WriteFile(fmt.Sprintf("%s/file1.js", dir), []byte("content"), 0644)
	os.WriteFile(fmt.Sprintf("%s/file2.txt", dir), []byte("content"), 0644)
	os.WriteFile(fmt.Sprintf("%s/file3.js", dir), []byte("content"), 0644)

	// Create a subdirectory with files
	subdir := fmt.Sprintf("%s/subdir", dir)
	fatalIfSet(os.Mkdir(subdir, 0755))
	os.WriteFile(fmt.Sprintf("%s/file4.js", subdir), []byte("content"), 0644)

	// Create an ignored directory
	ignoredDir := fmt.Sprintf("%s/ignored", dir)
	fatalIfSet(os.Mkdir(ignoredDir, 0755))
	os.WriteFile(fmt.Sprintf("%s/file5.js", ignoredDir), []byte("content"), 0644)

	t.Run("scans and filters by extension", func(t *testing.T) {
		files, err := ScanForFiles(dir,
			func(filename string) bool { return false },
			func(filename string) bool { return filepath.Ext(filename) == ".js" })

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Should find 4 .js files (file1.js, file3.js, file4.js, ignored/file5.js)
		if len(files) != 4 {
			t.Fatalf("expected 4 files, got %d", len(files))
		}
	})

	t.Run("respects ignore filter", func(t *testing.T) {
		files, err := ScanForFiles(dir,
			func(path string) bool {
				// Ignore the 'ignored' directory by checking if the path contains it
				return filepath.Base(path) == "ignored"
			},
			func(filename string) bool { return filepath.Ext(filename) == ".js" })

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Should find 3 .js files (file1.js, file3.js, file4.js) - not file5.js in ignored dir
		if len(files) != 3 {
			t.Fatalf("expected 3 files, got %d: %v", len(files), files)
		}
	})

	t.Run("captures modification times", func(t *testing.T) {
		files, err := ScanForFiles(dir,
			func(filename string) bool { return false },
			func(filename string) bool { return true })

		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		for path, modTime := range files {
			if modTime.IsZero() {
				t.Fatalf("file %s has zero modification time", path)
			}
		}
	})
}

// Test that files added to newly created directories are detected and synced
// When a new directory is created and files are added to it, those files should
// be detected and synced. The watcher now handles both Write and Create events,
// automatically adding newly created directories to the watch list.
func TestFilesInNewDirectoriesAreSynced(t *testing.T) {
	dir := fmt.Sprintf("%s/testNewDir.%d.%d", os.TempDir(), os.Getpid(), rand.Int())
	fatalIfSet(os.Mkdir(dir, 0755))
	defer os.RemoveAll(dir)

	// Create initial file so directory is not empty
	os.WriteFile(fmt.Sprintf("%s/existing.js", dir), []byte("initial"), 0644)

	// Set up watcher and sync
	watcher, err := fsnotify.NewWatcher()
	fatalIfSet(err)

	sync := &Sync{
		Base:        dir,
		WatchedExts: []string{"js"},
		Ignore:      []string{},
		watcher:     watcher,
		fileUpdated: make(chan string, 10),
	}

	// Watch the base directory
	fatalIfSet(sync.WatchDirectory(dir))

	// Start watching for file updates in a goroutine
	// Use a channel to detect when the goroutine encounters an error
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				// Expected panic when watcher is closed
			}
		}()
		sync.WaitForFileUpdates()
	}()

	// Ensure watcher is closed when test completes
	defer watcher.Close()

	// Give the watcher time to initialize
	time.Sleep(100 * time.Millisecond)

	// Create a new subdirectory
	newDir := fmt.Sprintf("%s/newsubdir", dir)
	fatalIfSet(os.Mkdir(newDir, 0755))

	// Give filesystem time to process directory creation
	time.Sleep(100 * time.Millisecond)

	// Add a file to the newly created directory
	newFile := fmt.Sprintf("%s/newfile.js", newDir)
	fatalIfSet(os.WriteFile(newFile, []byte("new content"), 0644))

	// Wait for the file to be detected
	select {
	case detected := <-sync.fileUpdated:
		// Normalize paths for comparison (handle different path separators and cleanup)
		expectedPath := filepath.Clean(newFile)
		detectedPath := filepath.Clean(detected)
		if detectedPath != expectedPath {
			t.Fatalf("expected %s to be detected, got %s", expectedPath, detectedPath)
		}
		// Success - file in new directory was detected
	case <-time.After(2 * time.Second):
		t.Fatal("file added to newly created directory was not detected within 2 seconds")
	}
}

// Test that a directory can be deleted and re-created without duplicate watching issues
func TestDirectoryDeleteAndRecreate(t *testing.T) {
	baseDir := fmt.Sprintf("%s/testDelRecreate.%d.%d", os.TempDir(), os.Getpid(), rand.Int())
	fatalIfSet(os.Mkdir(baseDir, 0755))
	defer os.RemoveAll(baseDir)

	// Create an initial watched file in the base directory
	os.WriteFile(fmt.Sprintf("%s/existing.txt", baseDir), []byte("initial"), 0644)

	// Create a subdirectory that we'll delete and recreate
	subDir := fmt.Sprintf("%s/subdir", baseDir)
	fatalIfSet(os.Mkdir(subDir, 0755))

	watcher, err := fsnotify.NewWatcher()
	fatalIfSet(err)

	sync := &Sync{
		Base:        baseDir,
		WatchedExts: []string{"txt"},
		Ignore:      []string{},
		watcher:     watcher,
		fileUpdated: make(chan string, 10),
	}

	// Watch both directories
	fatalIfSet(sync.WatchDirectory(baseDir))

	// Start watching for file updates
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Expected panic when watcher is closed
			}
		}()
		sync.WaitForFileUpdates()
	}()
	defer watcher.Close()

	time.Sleep(100 * time.Millisecond)

	// Delete the subdirectory
	fatalIfSet(os.RemoveAll(subDir))
	time.Sleep(100 * time.Millisecond)

	// Recreate the subdirectory
	fatalIfSet(os.Mkdir(subDir, 0755))
	time.Sleep(100 * time.Millisecond)

	// Add a file to the recreated directory
	testFile := fmt.Sprintf("%s/test.txt", subDir)
	fatalIfSet(os.WriteFile(testFile, []byte("content"), 0644))

	// Should receive exactly one event for the file
	eventCount := 0
	for {
		select {
		case <-time.After(50 * time.Millisecond):
			if eventCount == 0 {
				t.Fatal("file in recreated directory was not detected. You may need to increase the test timeout.")
			}
			return
		case <-sync.fileUpdated:
			eventCount++
			if eventCount > 1 {
				t.Fatalf("received %d events for file in recreated directory, expected 1", eventCount)
			}
		}
	}
}
