package coreapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A scope refusal is read both as the contract's scope_denied and as the 403 forbidden
// "token is missing a scope" the pinned core's routes answer, whose text comes in
// message rather than detail.
func TestIsScopeDenied(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusForbidden, `{"code":"scope_denied","detail":"ai:use"}`, true},
		{http.StatusForbidden, `{"code":"forbidden","message":"token is missing a scope"}`, true},
		{http.StatusForbidden, `{"code":"forbidden","message":"not on the matter's wall"}`, false},
		{http.StatusNotFound, `{"code":"not_found"}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			fmt.Fprint(w, tc.body)
		}))
		c := &Client{Base: srv.URL, Token: "test-token-" + t.Name(), HTTP: srv.Client()}
		err := c.Call(context.Background(), http.MethodPost, "/api/v1/ai/calls", Auth{}, "", map[string]any{}, nil)
		srv.Close()
		if got := IsScopeDenied(err); got != tc.want {
			t.Errorf("%d %s: IsScopeDenied = %v (%v)", tc.status, tc.body, got, err)
		}
	}
}
