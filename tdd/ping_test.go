package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendPingMakesPostRequest(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    })
	httptest.NewServer(handler)

	SendPing()
}
