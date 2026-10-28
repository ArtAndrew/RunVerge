// SPDX-License-Identifier: Elastic-2.0
// Container readiness probe. It only reads this container's fixed API endpoint.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"
)

func ready(client *http.Client, endpoint string) bool {
	response, err := client.Get(endpoint)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(body) > 4096 {
		return false
	}
	var result struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	return json.Unmarshal(body, &result) == nil && result.Data.Status == "ready"
}

func main() {
	client := &http.Client{
		Timeout:       2 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if !ready(client, "http://127.0.0.1:8080/readyz") {
		os.Exit(1)
	}
}
