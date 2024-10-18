package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendPingMakesPostRequest(t *testing.T) {
	requests := make(chan *http.Request, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
    })
	server := httptest.NewServer(handler)

	err := SendPing(server.URL)
	if err != nil { t.Error("SendPing raised an error") }

	select {
	case request := <- requests:
		if m := request.Method; m != http.MethodPost {
			t.Errorf("Expected 'POST' request, got '%s'", m)
		}
	default:
		t.Fatal("No request was captured")
	}
}
