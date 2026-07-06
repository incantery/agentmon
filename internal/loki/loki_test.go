package loki

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPushShapeAndHeaders(t *testing.T) {
	var gotPath, gotTenant, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTenant = r.Header.Get("X-Scope-OrgID")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "seth")
	ts := time.Date(2026, 7, 8, 10, 0, 0, 123, time.UTC)
	err := c.Push([]Stream{{
		Labels:  map[string]string{"job": "agentmon", "machine": "m1", "type": "user_prompt"},
		Entries: []Entry{{TS: ts, Line: []byte(`{"x":1}`)}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/loki/api/v1/push" || gotTenant != "seth" || gotCT != "application/json" {
		t.Errorf("path=%q tenant=%q ct=%q", gotPath, gotTenant, gotCT)
	}
	var body struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Streams) != 1 || body.Streams[0].Stream["machine"] != "m1" {
		t.Fatalf("body: %s", gotBody)
	}
	v := body.Streams[0].Values[0]
	if v[0] != "1783504800000000123" || v[1] != `{"x":1}` {
		t.Errorf("value = %v", v)
	}
}

func TestNoTenantHeaderWhenEmpty(t *testing.T) {
	var has bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, has = r.Header["X-Scope-Orgid"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	if err := New(srv.URL, "").Push([]Stream{{Labels: map[string]string{"job": "a"}, Entries: []Entry{{TS: time.Unix(1, 0), Line: []byte("{}")}}}}); err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("tenant header must be absent when tenant is empty")
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status    int
		permanent bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnprocessableEntity, true},
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "nope", tc.status)
		}))
		err := New(srv.URL, "").Push([]Stream{{Labels: map[string]string{"job": "a"}, Entries: []Entry{{TS: time.Unix(1, 0), Line: []byte("{}")}}}})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: want error", tc.status)
		}
		var perm *PermanentError
		if got := errors.As(err, &perm); got != tc.permanent {
			t.Errorf("status %d: permanent=%v, want %v (err=%v)", tc.status, got, tc.permanent, err)
		}
	}
}
