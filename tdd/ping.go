package main

import "net/http"

func SendPing(url string) error {
	http.Get(url)
	return nil
}
