package main

import (
	"fmt"
	"net/http"
)

func SendPing(serverUrl, attendance_id string) error {
	pingUrl := fmt.Sprintf("%s/attendances/%s/pings", serverUrl, attendance_id)
	http.Post(pingUrl, "application/text", nil)
	return nil
}
