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
	goldenSig  = "sha256=fc339ebda82c6735eb5c9c22c9232ab44625bef99088b4b91734fbddd1d36cff"
	goldenRes  = `{"request_id":"r1","status":"fail","summary":"exit 1","log_url":""}`
)

func TestGoldenWire(t *testing.T) {
	r, err := runnerproto.Decode([]byte(goldenBody))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(r); string(got) != goldenBody {
		t.Errorf("request bytes:\n%s", got)
	}
	secret := []byte("runner-secret-0123456789")
	if got := runnerproto.Sign(secret, []byte(goldenBody)); got != goldenSig {
		t.Errorf("signature %s", got)
	}
	if !runnerproto.Verify(secret, []byte(goldenBody), goldenSig) || runnerproto.Verify(secret, []byte(goldenBody+" "), goldenSig) {
		t.Error("verify accepts a changed body or rejects the golden signature")
	}
	if got, _ := json.Marshal(runnerproto.Result{RequestID: "r1", Status: runnerproto.Fail, Summary: "exit 1"}); string(got) != goldenRes {
		t.Errorf("result bytes: %s", got)
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
	for name, body := range map[string]string{"Request": goldenBody, "Result": goldenRes} {
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
