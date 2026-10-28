// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"agentplatform.local/platform/internal/capabilities"
	"agentplatform.local/platform/internal/domain"
	"net/http"
	"time"
)

func (s *Server) productCapabilities(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	if !s.materialQuery(w, r) {
		return
	}
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		s.fail(w, r, domain.Err("invalid_request", "Catalogue accepts no request body", 400))
		return
	}
	v, err := capabilities.Observe(p, time.Now())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, 200, v, "")
}
