// SPDX-License-Identifier: Elastic-2.0
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"agentplatform.local/platform/internal/domain"
)

func (s *Server) reviewTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var input domain.ReviewTaskRequest
	if !s.decode(w, r, &input) {
		return
	}
	value, e := s.Store.ReviewTask(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), input)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) retryTask(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var input domain.RetryTaskRequest
	if !s.decode(w, r, &input) {
		return
	}
	value, e := s.Store.RetryTask(r.Context(), p, r.PathValue("id"), r.Header.Get("Idempotency-Key"), input)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, value, "")
}
func (s *Server) candidates(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	values, next, e := s.Store.Candidates(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) decisions(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	values, next, e := s.Store.Decisions(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, values, next)
}
func (s *Server) children(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	n, e := limit(r)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	values, next, e := s.Store.Children(r.Context(), p, r.PathValue("id"), r.URL.Query().Get("cursor"), n)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	s.ok(w, 200, values, next)
}

// encoding/json deliberately accepts case-insensitive struct names. Mutation
// contracts do not: check exact tags recursively before decoding typed input.
func canonicalMutationFields(raw json.RawMessage, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.PkgPath() == "time" || typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if e := json.Unmarshal(raw, &object); e != nil {
			return e
		}
		if typ == reflect.TypeOf(domain.CreateTaskAutomation{}) {
			var value string
			if json.Unmarshal(object["max_charge_microusd"], &value) != nil {
				return fmt.Errorf("automation money must be a canonical decimal string")
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || strconv.FormatInt(n, 10) != value {
				return fmt.Errorf("automation money must be a canonical decimal string")
			}
		}
		// Omission inherits the parent goal. An explicitly supplied empty task
		// must not collapse into the same Go zero value and bypass the schema.
		if typ == reflect.TypeOf(domain.TeamMember{}) {
			if value, present := object["task"]; present {
				var task string
				if e := json.Unmarshal(value, &task); e != nil || strings.TrimSpace(task) == "" {
					return fmt.Errorf("member task must be nonblank when present")
				}
			}
		}
		if typ == reflect.TypeOf(domain.CreateMaterial{}) || typ == reflect.TypeOf(domain.ReviseMaterial{}) {
			if _, present := object["content"]; !present {
				return fmt.Errorf("material content must be present")
			}
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			name := tag[0]
			if name != "" && name != "-" {
				fields[name] = field.Type
				value, present := object[name]
				optional, quotedInteger := false, false
				for _, option := range tag[1:] {
					optional = optional || option == "omitempty"
					quotedInteger = quotedInteger || option == "string"
				}
				if configurationCommandShape(typ.Name()) && !optional && !present {
					return fmt.Errorf("configuration commands require every nonoptional field")
				}
				if present && quotedInteger && field.Type.Kind() >= reflect.Int && field.Type.Kind() <= reflect.Int64 {
					var decimal string
					if json.Unmarshal(value, &decimal) != nil {
						return fmt.Errorf("integer must be a canonical decimal string")
					}
					n, err := strconv.ParseInt(decimal, 10, field.Type.Bits())
					if err != nil || strconv.FormatInt(n, 10) != decimal {
						return fmt.Errorf("integer must be a canonical decimal string")
					}
				}
			}
		}
		for name, value := range object {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown field")
			}
			if e := canonicalMutationFields(value, field); e != nil {
				return e
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if e := json.Unmarshal(raw, &values); e != nil {
			return e
		}
		for _, value := range values {
			if e := canonicalMutationFields(value, typ.Elem()); e != nil {
				return e
			}
		}
	}
	return nil
}

func configurationCommandShape(name string) bool {
	switch name {
	case "CreateConfigurationGrant", "RevokeConfigurationGrant", "CreateConfigurationPlan", "ValidateConfigurationPlan", "ApproveConfigurationPlan", "ApplyConfigurationPlan", "StartConfigurationVerification", "ConfigurationOperationInput", "ConfigurationDestinationRef":
		return true
	}
	return false
}
