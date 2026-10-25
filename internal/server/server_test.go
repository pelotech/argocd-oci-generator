package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pelotech/argocd-oci-generator/internal/generator"
)

type fakeSource struct {
	in  generator.Inputs
	err error
}

func (f *fakeSource) Parameters(_ context.Context, in generator.Inputs) ([]generator.Entry, error) {
	f.in = in
	return []generator.Entry{{Name: in.Repositories[0], Tag: "3.0.10"}}, f.err
}

func call(t *testing.T, srv *httptest.Server, token, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/getparams.execute", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHandler(t *testing.T) {
	source := &fakeSource{}
	srv := httptest.NewServer(Handler(source, "plugin-token"))
	defer srv.Close()
	body := `{"applicationSetName": "demo", "input": {"parameters": {"repositories": ["services/payments"], "tag": "3.x", "keep": {"count": 2}, "file": "config.json"}}}`
	if resp := call(t, srv, "", body); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without the token: %s", resp.Status)
	}
	resp := call(t, srv, "plugin-token", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("with the token: %s", resp.Status)
	}
	var out struct {
		Output struct {
			Parameters []generator.Entry `json:"parameters"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Output.Parameters) != 1 || out.Output.Parameters[0].Tag != "3.0.10" {
		t.Errorf("answer: %+v %v", out, err)
	}
	if source.in.Tag != "3.x" || *source.in.Keep.Count != 2 || source.in.File != "config.json" {
		t.Errorf("inputs: %+v", source.in)
	}
	source.err = errors.New("registry down")
	if resp := call(t, srv, "plugin-token", body); resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a failed poll answers 502, got %s", resp.Status)
	}
	source.err = nil
	if resp := call(t, srv, "plugin-token", `{"input": {"parameters": {"repositories": ["`+strings.Repeat("a", MaxBody)+`"]}}}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a body over MaxBody answers 400, got %s", resp.Status)
	}
	empty := httptest.NewServer(Handler(source, ""))
	defer empty.Close()
	if resp := call(t, empty, "", body); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an empty token refuses every call, got %s", resp.Status)
	}
}
