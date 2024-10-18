package main

import "net/http"

func SendPing(url string) error {
	http.Post(url, "application/text", nil)
	return nil
}
