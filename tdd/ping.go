package main

import "net/http"

func SendPing(url, attendance_id string) error {
	http.Post(url, "application/text", nil)
	return nil
}
