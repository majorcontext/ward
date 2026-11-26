package runnerproto_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/majorcontext/ward/runnerproto"
)

const (
	goldenBody = `{"request_id":"r1","repo":"native:widgets","tree":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","commit":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","check":{"name":"unit","run":["make","test"],"timeout_seconds":60},"check_hash":"9061f937a736957444b569a89e07f22c84d9d83e354fb1c5910d71311849ccf5","env":"env1"}`
	goldenRes  = `{"request_id":"r1","status":"fail","summary":"exit 1","log_url":""}`

	goldenLeaseRequest = `{"isolation":"gvisor"}`
	goldenLease        = `{"lease_id":"l1","ttl_seconds":60,"request":` + goldenBody + `}`
	goldenRenewal      = `{"ttl_seconds":60}`
)

func TestGoldenWire(t *testing.T) {
	r, err := runnerproto.Decode([]byte(goldenBody))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(r); string(got) != goldenBody {
		t.Errorf("request bytes:\n%s", got)
	}
	if got, _ := json.Marshal(runnerproto.Result{RequestID: "r1", Status: runnerproto.Fail, Summary: "exit 1"}); string(got) != goldenRes {
		t.Errorf("result bytes: %s", got)
	}
}

func TestGoldenPullWire(t *testing.T) {
	req, _ := runnerproto.Decode([]byte(goldenBody))
	for want, v := range map[string]any{
		goldenLeaseRequest: runnerproto.LeaseRequest{Isolation: runnerproto.IsolationGVisor},
		goldenLease:        runnerproto.Lease{LeaseID: "l1", TTLSeconds: 60, Request: req},
		goldenRenewal:      runnerproto.Renewal{TTLSeconds: 60},
	} {
		if got, _ := json.Marshal(v); string(got) != want {
			t.Errorf("bytes:\n%s\nwant\n%s", got, want)
		}
	}
	if runnerproto.RenewPath("l1") != "/v1/runner/leases/l1/renew" {
		t.Error("renew path")
	}
	if !runnerproto.IsolationGVisor.AtLeast(runnerproto.IsolationContainer) || runnerproto.IsolationContainer.AtLeast(runnerproto.IsolationGVisor) || runnerproto.Isolation("").AtLeast(runnerproto.IsolationContainer) {
		t.Error("isolation order: gvisor beats container, and an unknown level beats nothing")
	}
}

func TestDecodeRejectsWrongHash(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal([]byte(goldenBody), &m)
	m["check_hash"] = "00"
	raw, _ := json.Marshal(m)
	if _, err := runnerproto.Decode(raw); err == nil {
		t.Error("a request with a wrong check_hash decoded")
	}
}

func TestSchemaNamesTheWireFields(t *testing.T) {
	raw, err := os.ReadFile("runner.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs map[string]struct {
			Required []string `json:"required"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"Request": goldenBody, "Result": goldenRes, "LeaseRequest": goldenLeaseRequest, "Lease": goldenLease, "Renewal": goldenRenewal} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(body), &m)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		want := slices.Clone(s.Defs[name].Required)
		slices.Sort(want)
		if !slices.Equal(keys, want) {
			t.Errorf("%s: wire fields %v, schema requires %v", name, keys, want)
		}
	}
}
