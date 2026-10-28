// SPDX-License-Identifier: Elastic-2.0
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadyRequiresSuccessfulBoundedReadinessResponse(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"ready", 200, `{"data":{"status":"ready"}}`, true},
		{"admission_blocked", 503, `{"data":{"status":"ready"}}`, false},
		{"ordinary_page", 200, `<html>Welcome</html>`, false},
		{"not_ready", 200, `{"data":{"status":"starting"}}`, false},
		{"oversized", 200, `{"data":{"status":"ready"}}` + strings.Repeat(" ", 4096), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			if got := ready(server.Client(), server.URL); got != test.want {
				t.Fatalf("ready = %v, want %v", got, test.want)
			}
		})
	}
}
