package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendPingMakesPostRequest(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    })
	server := httptest.NewServer(handler)

	err := SendPing(server.URL)
}
