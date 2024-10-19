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

func validateRequest(t *testing.T, requests chan *http.Request, method, expectedUrl string) {
	select {
	case request := <-requests:
		if m := request.Method; m != method {
			t.Errorf("Expected '%s' request, got '%s'", method, m)
		}
		if url := request.URL.String(); url != expectedUrl {
			t.Errorf("Expected request to '%s', got '%s'", expectedUrl, url)
		}
	default:
		t.Fatal("No request was captured")
	}
}

func TestSendPingMakesPostRequest(t *testing.T) {
	server, requests := setupTestServer()
	defer server.Close()

	attendanceId := "attendance_id_123"
	err := SendPing(server.URL, attendanceId)
	if err != nil {
		t.Error("SendPing raised an error")
	}

	expectedUrl := "/attendances/attendance_id_123/pings"
	validateRequest(t, requests, http.MethodPost, expectedUrl)
}
