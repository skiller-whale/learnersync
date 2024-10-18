package main

import (
	"fmt"
	"net/http"
)

func SendPing(url, attendance_id string) error {
	pingUrl := fmt.Sprintf("%s/attendances/%s/pings", url, attendance_id)
	http.Post(pingUrl, "application/text", nil)
	return nil
}
