// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"agentplatform.local/platform/internal/domain"
	"agentplatform.local/platform/internal/identity"
)

func (s *Server) mountAuthentication(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/config", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			s.fail(w, r, domain.Err("invalid_request", "Некорректный запрос", 400))
			return
		}
		s.ok(w, 200, map[string]bool{"oidc_enabled": s.Login != nil}, "")
	})
	mux.HandleFunc("GET /auth/organizations", s.browserOrganizations)
	for _, path := range []string{identity.LoginStartPath, identity.LoginCallbackPath, identity.SessionPath, identity.SessionRotatePath, identity.LogoutPath} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if s.Login == nil {
				s.fail(w, r, domain.NotFound)
				return
			}
			// Browser login/session endpoints never interpret an Authorization header.
			if len(r.Header.Values("Authorization")) != 0 {
				s.fail(w, r, domain.Unauthenticated)
				return
			}
			s.Login.Handler().ServeHTTP(w, r)
		})
	}
	mux.HandleFunc("/auth/", func(w http.ResponseWriter, r *http.Request) { s.fail(w, r, domain.NotFound) })
}

func (s *Server) auth(fn authorizedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgs := r.Header.Values("X-Organization-ID")
		if len(orgs) != 1 || orgs[0] == "" {
			s.fail(w, r, domain.Unauthenticated)
			return
		}
		headers := r.Header.Values("Authorization")
		var p domain.Principal
		var err error
		if len(headers) > 0 {
			if len(headers) != 1 || hasSessionCookie(r) || !strings.HasPrefix(headers[0], "Bearer ") {
				s.fail(w, r, domain.Unauthenticated)
				return
			}
			token := strings.TrimPrefix(headers[0], "Bearer ")
			if token == "" || strings.ContainsAny(token, " \t\r\n,") {
				s.fail(w, r, domain.Unauthenticated)
				return
			}
			p, err = s.Store.Authenticate(r.Context(), token, orgs[0])
		} else {
			if s.Login == nil {
				s.fail(w, r, domain.Unauthenticated)
				return
			}
			var session identity.BrowserSession
			session, err = s.Login.SessionFromRequest(r.Context(), r)
			if err != nil {
				s.loginFailure(w, r, err)
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && s.Login.CheckCSRF(r) != nil {
				s.fail(w, r, domain.Forbidden)
				return
			}
			p, err = s.Store.AuthenticateBrowserSession(r.Context(), session.TokenHash, orgs[0])
			if err == nil && p.ID != session.PrincipalID {
				err = domain.Unauthenticated
			}
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !s.checkAPIContract(w, r, true) {
			return
		}
		fn(w, r, p)
	}
}

func hasSessionCookie(r *http.Request) bool {
	// Inspect raw names too: a malformed cookie must not silently switch the
	// request to Bearer authentication via net/http's lenient cookie parser.
	for _, line := range r.Header.Values("Cookie") {
		for _, item := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(item), "=")
			if strings.TrimSpace(name) == identity.SessionCookieName {
				return true
			}
		}
	}
	return false
}

func (s *Server) loginFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, identity.ErrLoginStoreUnavailable) {
		s.fail(w, r, domain.Err("unavailable", "Сервис входа временно недоступен", 503))
		return
	}
	s.fail(w, r, domain.Unauthenticated)
}

func (s *Server) browserOrganizations(w http.ResponseWriter, r *http.Request) {
	if s.Login == nil || len(r.Header.Values("Authorization")) != 0 {
		s.fail(w, r, domain.Unauthenticated)
		return
	}
	if r.Method != http.MethodGet || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		s.fail(w, r, domain.Err("invalid_request", "Некорректный запрос", 400))
		return
	}
	q, e := parseOrganizationQuery(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	session, e := s.Login.SessionFromRequest(r.Context(), r)
	if e != nil {
		s.loginFailure(w, r, e)
		return
	}
	items, next, e := s.Store.BrowserOrganizations(r.Context(), session.TokenHash, r.URL.Query().Get("cursor"), q)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, items, next)
}

func parseOrganizationQuery(r *http.Request) (int, error) {
	if len(r.URL.RawQuery) > 2048 {
		return 0, domain.Err("invalid_request", "Некорректный запрос", 400)
	}
	q, e := url.ParseQuery(r.URL.RawQuery)
	if e != nil {
		return 0, domain.Err("invalid_request", "Некорректный запрос", 400)
	}
	for k, v := range q {
		if (k != "cursor" && k != "limit") || len(v) != 1 {
			return 0, domain.Err("invalid_request", "Некорректный запрос", 400)
		}
	}
	return limit(r)
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	items, next, e := s.Store.Models(r.Context(), p, r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, items, next)
}
