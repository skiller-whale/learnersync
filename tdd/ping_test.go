package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func setupTestServer() (*httptest.Server, chan *http.Request) {
	requests := make(chan *http.Request, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
	})
	server := httptest.NewServer(handler)
	return server, requests
}


func TestSendPingMakesPostRequest(t *testing.T) {
	server, requests := setupTestServer()

	attendanceId := "attendance_id_123"
	err := SendPing(server.URL, attendanceId)
	if err != nil { t.Error("SendPing raised an error") }

	expectedUrl := "/attendances/attendance_id_123/pings"
	select {
	case request := <- requests:
		if m := request.Method; m != http.MethodPost {
			t.Errorf("Expected 'POST' request, got '%s'", m)
		}
		if url := request.URL.String(); url != expectedUrl {
			t.Errorf("Expected request to '%s', got '%s'", expectedUrl, url)
		}
	default:
		t.Fatal("No request was captured")
	}
}
