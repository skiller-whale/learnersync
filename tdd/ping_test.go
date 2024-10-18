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

	<- requests  // Get the first request
}
