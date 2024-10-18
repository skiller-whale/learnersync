package main

import (
	"net/http"
)

func SendPing(url, attendance_id string) error {
	http.Post(url + "/attendances/attendance_id_123/pings", "application/text", nil)
	return nil
}
